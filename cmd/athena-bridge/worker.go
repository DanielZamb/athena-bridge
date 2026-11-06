package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
)

type settings struct {
	Function, Region, Profile string
}

type lambdaAPI interface {
	Invoke(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type client struct {
	api      lambdaAPI
	function string
	region   string
}

type request struct {
	Version          int    `json:"version"`
	Action           string `json:"action"`
	RequestID        string `json:"request_id,omitempty"`
	Database         string `json:"database,omitempty"`
	Table            string `json:"table,omitempty"`
	SQL              string `json:"sql,omitempty"`
	QueryID          string `json:"query_id,omitempty"`
	NextToken        string `json:"next_token,omitempty"`
	PageSize         int    `json:"page_size,omitempty"`
	Term             string `json:"term,omitempty"`
	Kind             string `json:"kind,omitempty"`
	ExpectedResultID string `json:"expected_result_id,omitempty"`
}

type workerError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *workerError) Error() string { return e.Code + ": " + e.Message }

type envelope struct {
	Version int             `json:"version"`
	Result  json.RawMessage `json:"result"`
	Error   *workerError    `json:"error"`
}

type column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type page struct {
	QueryID   string      `json:"query_id,omitempty"`
	ResultID  string      `json:"result_id,omitempty"`
	Columns   []column    `json:"columns"`
	Rows      [][]*string `json:"rows"`
	NextToken string      `json:"next_token,omitempty"`
}

type started struct {
	QueryID string `json:"query_id"`
}

type queryStatus struct {
	QueryID    string           `json:"query_id"`
	State      string           `json:"state"`
	Reason     string           `json:"reason,omitempty"`
	Statistics *queryStatistics `json:"statistics,omitempty"`
	Failure    *queryFailure    `json:"failure,omitempty"`
}

type queryStatistics struct {
	DataScannedBytes  *string `json:"data_scanned_bytes,omitempty"`
	EngineExecutionMS *string `json:"engine_execution_ms,omitempty"`
	TotalExecutionMS  *string `json:"total_execution_ms,omitempty"`
	QueueMS           *string `json:"queue_ms,omitempty"`
	PlanningMS        *string `json:"planning_ms,omitempty"`
	ResultReused      *bool   `json:"result_reused,omitempty"`
}

type queryFailure struct {
	ErrorCategory *int64 `json:"error_category,omitempty"`
	ErrorType     *int64 `json:"error_type,omitempty"`
	Retryable     *bool  `json:"retryable,omitempty"`
	SQLLine       *int64 `json:"sql_line,omitempty"`
	SQLColumn     *int64 `json:"sql_column,omitempty"`
}

type cancelled struct {
	queryStatus
	CancellationRequested *bool `json:"cancellation_requested"`
}

type workerInfo struct {
	WorkerID        string           `json:"worker_id"`
	Basis           string           `json:"basis"`
	DefaultDatabase string           `json:"default_database"`
	Databases       []string         `json:"databases"`
	Catalog         string           `json:"catalog"`
	Workgroup       string           `json:"workgroup"`
	Actions         []string         `json:"actions"`
	Features        []string         `json:"features"`
	Limits          map[string]int64 `json:"limits"`
}

type download struct {
	URL       string `json:"url"`
	Format    string `json:"format"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
}

func newClient(ctx context.Context, s settings) (*client, error) {
	// Raw SDK diagnostics can contain private transport details and bypass JSONL.
	opts := []func(*config.LoadOptions) error{config.WithLogger(logging.Nop{})}
	if s.Region != "" {
		opts = append(opts, config.WithRegion(s.Region))
	}
	if s.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(s.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fault(CodeConfig, "configure", "cannot load AWS configuration", RecoveryConfig, err)
	}
	if cfg.Region == "" {
		return nil, fault(CodeConfig, "configure", "set --region, AWS_REGION, or a region in your AWS profile", RecoveryConfig, nil)
	}
	provider := cfg.Credentials
	cfg.Credentials = aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		creds, err := provider.Retrieve(ctx)
		if err != nil {
			return creds, fault(CodeAuthentication, "authenticate", "cannot retrieve AWS credentials", RecoveryConfig, err)
		}
		return creds, nil
	})
	return &client{api: lambda.NewFromConfig(cfg, func(o *lambda.Options) {
		o.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}), function: s.Function, region: cfg.Region}, nil
}

func (c *client) call(ctx context.Context, req request, result any) error {
	req.Version = 1
	payload, err := json.Marshal(req)
	if err != nil {
		return protocolFault(req.Action, "cannot encode worker request", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := c.api.Invoke(callCtx, &lambda.InvokeInput{
		FunctionName: aws.String(c.function), InvocationType: types.InvocationTypeRequestResponse,
		LogType: types.LogTypeNone, Payload: payload,
	})
	if err != nil {
		return invocationFault(req.Action, err)
	}
	if response == nil {
		return protocolFault(req.Action, "Lambda returned no response", nil)
	}
	if response.FunctionError != nil {
		return fault(CodeFunction, req.Action, "Lambda function failed; inspect its CloudWatch logs", RecoveryWorker, nil)
	}
	var reply envelope
	if err := decodeJSON(response.Payload, &reply); err != nil {
		return protocolFault(req.Action, "invalid worker response; expected one JSON value", err)
	}
	if reply.Version != 1 {
		return protocolFault(req.Action, "unsupported worker protocol version; expected 1", nil)
	}
	hasResult := len(reply.Result) != 0 && !bytes.Equal(bytes.TrimSpace(reply.Result), []byte("null"))
	if hasResult == (reply.Error != nil) {
		return protocolFault(req.Action, "worker response must contain exactly one of result or error", nil)
	}
	if reply.Error != nil {
		if reply.Error.Code == "" || reply.Error.Message == "" {
			return protocolFault(req.Action, "worker error requires code and message", nil)
		}
		code, recovery := CodeWorker, RecoveryWorker
		switch reply.Error.Code {
		case "NOT_READY":
			code, recovery = CodeNotReady, RecoveryWait
		case "RESULT_CHANGED":
			code, recovery = CodeResultChanged, RecoveryInspect
		case "NOT_ALLOWED":
			recovery = RecoveryNone
		case "INVALID_REQUEST":
			recovery = RecoveryArguments
		}
		return fault(code, req.Action, "worker rejected the request", recovery, reply.Error)
	}
	if err := decodeJSON(reply.Result, result); err != nil {
		return protocolFault(req.Action, "invalid worker result", err)
	}
	return nil
}

func invocationFault(operation string, err error) *Fault {
	f := fault(CodeTransport, operation, "cannot invoke Lambda", RecoveryRetry, err)
	var credentialFault *Fault
	var apiError interface{ ErrorCode() string }
	var signing *v4.SigningError
	var region *aws.MissingRegionError
	var params smithy.InvalidParamsError
	switch {
	case errors.As(err, &credentialFault) && credentialFault.Code == CodeAuthentication:
		f.Code = CodeAuthentication
	case errors.As(err, &apiError):
		switch apiError.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedException", "UnauthorizedOperation", "EC2AccessDeniedException", "KMSAccessDeniedException":
			f.Code = CodeAuthorization
		case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "UnrecognizedClientException", "InvalidSignatureException", "SignatureDoesNotMatch", "MissingAuthenticationToken", "MissingAuthenticationTokenException", "IncompleteSignature", "IncompleteSignatureException", "RequestExpired", "AuthFailure":
			f.Code = CodeAuthentication
		case "ResourceNotFoundException", "InvalidParameterValueException", "InvalidRequestContentException", "InvalidRuntimeException", "InvalidSecurityGroupIDException", "InvalidSubnetIDException", "KMSDisabledException", "KMSInvalidStateException", "KMSNotFoundException", "NoPublishedVersionException", "UnsupportedMediaTypeException", "RequestTooLargeException":
			f.Code = CodeConfig
		default:
			if (retry.ThrottleErrorCode{Codes: retry.DefaultThrottleErrorCodes}).IsErrorThrottle(err) == aws.TrueTernary {
				f.Code = CodeThrottled
			}
		}
	case errors.As(err, &signing):
		f.Code = CodeAuthentication
	case errors.As(err, &region), errors.As(err, &params):
		f.Code = CodeConfig
	}
	switch f.Code {
	case CodeAuthorization:
		f.Message, f.Recovery = "Lambda invocation was denied; check configured access", RecoveryConfig
	case CodeAuthentication:
		f.Message, f.Recovery = "cannot authenticate AWS request; check the configured credential provider", RecoveryConfig
	case CodeConfig:
		f.Message, f.Recovery = "check Lambda invocation configuration", RecoveryConfig
	case CodeThrottled:
		f.Message = "Lambda invocation was throttled; retry later"
	}
	return f
}

func decodeJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func validatePage(p page) error {
	if p.Columns == nil || p.Rows == nil {
		return protocolFault("page", "worker page requires columns and rows arrays", nil)
	}
	for _, c := range p.Columns {
		if c.Name == "" || c.Type == "" {
			return protocolFault("page", "each worker column requires a name and type", nil)
		}
	}
	for i, row := range p.Rows {
		if row == nil || len(row) != len(p.Columns) {
			return protocolFault("page", fmt.Sprintf("worker row %d has %d cells for %d columns", i+1, len(row), len(p.Columns)), nil)
		}
	}
	return nil
}
