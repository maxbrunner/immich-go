package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/fshelper"
)

const (
	EndPointGetJobs                = "GetJobs"
	EndPointSendJobCommand         = "SendJobCommand"
	EndPointCreateJob              = "CreateJob"
	EndPointGetAllAlbums           = "GetAllAlbums"
	EndPointGetAlbumInfo           = "GetAlbumInfo"
	EndPointAddAsstToAlbum         = "AddAssetToAlbum"
	EndPointCreateAlbum            = "CreateAlbum"
	EndPointGetAssetAlbums         = "GetAssetAlbums"
	EndPointDeleteAlbum            = "DeleteAlbum"
	EndPointPingServer             = "PingServer"
	EndPointValidateConnection     = "ValidateConnection"
	EndPointGetServerStatistics    = "GetServerStatistics"
	EndPointGetAssetStatistics     = "GetAssetStatistics"
	EndPointGetSupportedMediaTypes = "GetSupportedMediaTypes"
	EndPointGetAllAssets           = "GetAllAssets"
	EndPointUpsertTags             = "UpsertTags"
	EndPointTagAssets              = "TagAssets"
	EndPointBulkTagAssets          = "BulkTagAssets"
	EndPointGetAllTags             = "GetAllTags"
	EndPointAssetUpload            = "AssetUpload"
	EndPointAssetReplace           = "AssetReplace"
	EndPointCopyAsset              = "CopyAsset"
	EndPointGetAboutInfo           = "GetAboutInfo"
	EndPointGetSearchSuggestions   = "GetSearchSuggestions"
	EndPointGetAllPeople           = "GetAllPeople"
	EndPointSignUpAdmin            = "SignUpAdmin"
	EndPointAdminLogin             = "AdminLogin"
	EndPointLogin                  = "Login"
	EndPointGetUserInfo            = "GetUserInfo"
	EndPointUpdateAdminOnboarding  = "UpdateAdminOnboarding"
	EndPointCreateApiKey           = "CreateApiKey"
)

type TooManyInternalError struct {
	error
}

func (e TooManyInternalError) Is(target error) bool {
	_, ok := target.(*TooManyInternalError)
	return ok
}

// serverCall permit to decorate request and responses in one line
type serverCall struct {
	endPoint           string
	ic                 *ImmichClient
	err                error
	ctx                context.Context
	hasResponseHandler bool
	attempts           int // how many tries a failing call got, so the error can say
}

func (ic *ImmichClient) newServerCall(ctx context.Context, api string) *serverCall {
	sc := &serverCall{
		endPoint: api,
		ic:       ic,
		ctx:      ctx,
	}
	return sc
}

func (sc *serverCall) Err(req *http.Request, resp *http.Response, msg serverError) error {
	ce := callError{
		endPoint: sc.endPoint,
		err:      sc.err,
		attempts: sc.attempts,
	}
	if req != nil {
		ce.method = req.Method
		ce.url = req.URL.String()
	}
	if resp != nil {
		ce.status = resp.StatusCode
	}
	ce.message = msg
	return ce
}

// Builds the correct error message based on server version
func (sc *serverCall) decodeServerError(resp *http.Response) serverError {
	var body []byte
	if resp.Body != nil {
		defer resp.Body.Close()
		if isJSON(resp.Header.Get("Content-Type")) {
			body, _ = io.ReadAll(resp.Body)
		}
	}

	if v := sc.ic.serverVersion; v != nil && v.Major() < 3 {
		var e serverErrorV2
		json.Unmarshal(body, &e)
		return e
	}

	e := serverErrorV3{CorrelationID: resp.Header.Get("X-Correlation-ID")}
	json.Unmarshal(body, &e)
	return e
}

func (sc *serverCall) joinError(err error) error {
	sc.err = errors.Join(sc.err, err)
	return err
}

type requestFunction func(sc *serverCall) *http.Request

var callSequence atomic.Int64

type callSequenceID string

const ctxCallSequenceID callSequenceID = "api-call-sequence"

func (sc *serverCall) request(
	method string,
	url string,
	opts ...serverRequestOption,
) *http.Request {
	if sc.ic.apiTraceWriter != nil && sc.endPoint != EndPointGetJobs {
		seq := callSequence.Add(1)
		sc.ctx = context.WithValue(sc.ctx, ctxCallSequenceID, seq)
	}
	req, err := http.NewRequestWithContext(sc.ctx, method, url, nil)
	if sc.joinError(err) != nil {
		return nil
	}
	opts = append(opts, setAPIKey())
	for _, opt := range opts {
		if opt != nil {
			if sc.joinError(opt(sc, req)) != nil {
				return nil
			}
		}
	}
	return req
}

func getRequest(url string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(http.MethodGet, sc.ic.endPoint+url, opts...)
	}
}

func postRequest(url string, cType string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(
			http.MethodPost,
			sc.ic.endPoint+url,
			append(opts, setContentType(cType))...)
	}
}

func deleteRequest(url string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(http.MethodDelete, sc.ic.endPoint+url, opts...)
	}
}

func putRequest(url string, opts ...serverRequestOption) requestFunction {
	return func(sc *serverCall) *http.Request {
		if sc.err != nil {
			return nil
		}
		return sc.request(http.MethodPut, sc.ic.endPoint+url, opts...)
	}
}

// retriableStatuses are transient by nature: the request was well-formed and
// resending it later stands a good chance of succeeding. A hosted Immich behind
// a gateway returns 502/504 whenever the upstream stalls, and one of those
// should not cost a multi-hour archive run.
var retriableStatuses = map[int]bool{
	http.StatusTooManyRequests:     true, // 429
	http.StatusInternalServerError: true, // 500
	http.StatusBadGateway:          true, // 502
	http.StatusServiceUnavailable:  true, // 503
	http.StatusGatewayTimeout:      true, // 504
}

// replayable reports whether the request can safely be sent again: either it
// carries no body, or it can regenerate one. Uploads set a body without
// GetBody, so they are excluded.
func replayable(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	return req.GetBody != nil
}

// replayBody regenerates a request body for a repeat attempt.
func replayBody(req *http.Request) (io.ReadCloser, error) {
	if req.GetBody == nil {
		return nil, nil
	}
	return req.GetBody()
}

// drain lets the connection be reused rather than abandoned between attempts.
func drain(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// waitBeforeRetry backs off, but gives up immediately if the caller is done.
func (sc *serverCall) waitBeforeRetry(attempt int) error {
	delay := sc.ic.RetriesDelay
	if delay <= 0 {
		delay = time.Second
	}
	select {
	case <-time.After(time.Duration(attempt) * delay):
		return nil
	case <-sc.ctx.Done():
		return sc.ctx.Err()
	}
}

func (sc *serverCall) do(fnRequest requestFunction, opts ...serverResponseOption) error {
	var (
		resp *http.Response
		err  error
	)

	req := fnRequest(sc)
	if sc.err != nil || req == nil {
		return sc.Err(req, nil, nil)
	}

	// Transient failures are retried rather than surfaced. The Retries and
	// RetriesDelay fields have existed on the client since the beginning,
	// documented as "number of attempts on 500 errors", but nothing ever read
	// them: a single 502 failed the call outright.
	attempts := sc.ic.Retries
	if attempts < 0 {
		attempts = 0
	}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			body, gerr := replayBody(req)
			if gerr != nil {
				sc.err = gerr
				return sc.Err(req, nil, nil)
			}
			req.Body = body
			if werr := sc.waitBeforeRetry(attempt); werr != nil {
				sc.err = werr
				return sc.Err(req, nil, nil)
			}
		}

		resp, err = sc.ic.client.Do(req)

		transient := err != nil || (resp != nil && retriableStatuses[resp.StatusCode])
		if transient && attempt < attempts && replayable(req) {
			drain(resp)
			continue
		}

		// any non nil error must be returned
		if err != nil {
			sc.err = err
			return sc.Err(req, nil, nil)
		}

		// Any StatusCode above 300 denotes a problem, we expect a JSON with the server's error
		if resp.StatusCode >= 300 {
			sc.attempts = attempt + 1
			return sc.Err(req, resp, sc.decodeServerError(resp))
		}
		break
	}

	// We have a success
	for _, opt := range opts {
		if opt != nil {
			_ = sc.joinError(opt(sc, resp))
		}
	}
	if !sc.hasResponseHandler && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if sc.err != nil {
		return sc.Err(req, resp, nil)
	}
	return nil
}

type serverRequestOption func(sc *serverCall, req *http.Request) error

func setBody(body io.ReadCloser) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Body = body
		return nil
	}
}

func setImmichChecksum(a *assets.Asset) serverRequestOption {
	if a.Checksum == "" {
		return nil
	}
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Set("x-immich-checksum", a.Checksum)
		return nil
	}
}

func setAcceptJSON() serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Add("Accept", "application/json")
		return nil
	}
}

func setOctetStream() serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Add("Accept", "application/octet-stream")
		return nil
	}
}

func setAPIKey() serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Set("x-api-key", sc.ic.key)
		return nil
	}
}

func setJSONBody(object any) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		b := bytes.NewBuffer(nil)
		enc := json.NewEncoder(b)
		err := enc.Encode(object)
		if err != nil {
			return err
		}
		// GetBody makes the request replayable, which is what qualifies it for
		// a retry. It matters most for /search/metadata: a transient 502 there
		// aborts the whole enumeration rather than losing a single asset.
		// Uploads use setBody, which deliberately leaves GetBody nil, so they
		// are never retried.
		body := b.Bytes()
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.Header.Set("Content-Type", "application/json")
		return err
	}
}

func setContentType(cType string) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		req.Header.Set("Content-Type", cType)
		return nil
	}
}

type URLRequester interface {
	SetURL(u *url.URL) error
}

func UrlRequest(ur URLRequester) serverRequestOption {
	return func(sc *serverCall, req *http.Request) error {
		if ur != nil {
			return ur.SetURL(req.URL)
		}
		return nil
	}
}

type serverResponseOption func(sc *serverCall, resp *http.Response) error

func responseJSON[T any](object *T) serverResponseOption {
	return func(sc *serverCall, resp *http.Response) error {
		sc.hasResponseHandler = true
		if resp != nil {
			if resp.Body != nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					return nil
				}
				err := json.NewDecoder(resp.Body).Decode(object)
				if err != nil {
					err = fmt.Errorf("can't decode JSON response: %w", err)
				}
				return err
			}
		}
		return errors.New("can't decode nil response")
	}
}

func responseCopy(buffer *bytes.Buffer) serverResponseOption {
	return func(sc *serverCall, resp *http.Response) error {
		sc.hasResponseHandler = true
		if resp != nil {
			if resp.Body != nil {
				newBody := fshelper.TeeReadCloser(resp.Body, buffer)
				resp.Body = newBody
				return nil
			}
		}
		return nil
	}
}

func responseOctetStream(rc *io.ReadCloser) serverResponseOption {
	return func(sc *serverCall, resp *http.Response) error {
		sc.hasResponseHandler = true
		if resp != nil {
			if resp.Body != nil {
				*rc = resp.Body
				return nil
			}
		}
		return nil
	}
}

func isJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}
