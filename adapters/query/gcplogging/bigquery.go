package gcplogging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// This file is the whole BigQuery jobs.query surface the adapter needs:
// insert a query, poll it if it did not finish inline, and page the rows
// out. It is plain JSON over HTTPS — the reason no cloud SDK is required.

// queryParam is one NAMED query parameter. Values reach BigQuery here and
// only here; nothing caller-supplied is ever rendered into the SQL text.
type queryParam struct {
	Name          string        `json:"name"`
	ParameterType paramType     `json:"parameterType"`
	ParameterVal  paramValueBox `json:"parameterValue"`
}

type paramType struct {
	Type string `json:"type"`
}

type paramValueBox struct {
	Value string `json:"value"`
}

func stringParam(name, value string) queryParam {
	return queryParam{
		Name:          name,
		ParameterType: paramType{Type: "STRING"},
		ParameterVal:  paramValueBox{Value: value},
	}
}

// bqTimestampLayout is BigQuery's canonical TIMESTAMP literal. Microsecond
// precision is BigQuery's own: a TIMESTAMP column cannot hold the
// nanosecond tail a Cloud Logging entry may carry, so an outcome's
// sub-microsecond component does not survive the round trip. The engine's
// windows are minutes to hours, so this changes no reported figure; it is
// recorded here because it is a real property of this read path.
const bqTimestampLayout = "2006-01-02 15:04:05.000000-07:00"

func timestampParam(name string, t time.Time) queryParam {
	return queryParam{
		Name:          name,
		ParameterType: paramType{Type: "TIMESTAMP"},
		ParameterVal:  paramValueBox{Value: t.UTC().Format(bqTimestampLayout)},
	}
}

// queryRequest is the jobs.query request body.
type queryRequest struct {
	Query           string       `json:"query"`
	UseLegacySQL    bool         `json:"useLegacySql"`
	ParameterMode   string       `json:"parameterMode"`
	QueryParameters []queryParam `json:"queryParameters,omitempty"`
	TimeoutMs       int64        `json:"timeoutMs"`
	MaxResults      int64        `json:"maxResults"`
	Location        string       `json:"location,omitempty"`
}

// bqCell is one column value. BigQuery renders every scalar as a JSON
// string, INT64 included (a JSON number would lose precision above 2^53 —
// the same reason money never rides a float here).
type bqCell struct {
	V string `json:"v"`
}

// bqRow is one result row.
type bqRow struct {
	F []bqCell `json:"f"`
}

// jobRef identifies a query job that did not complete inline.
type jobRef struct {
	JobID    string `json:"jobId"`
	Location string `json:"location"`
}

// apiStatus is Google's error envelope.
type apiStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// queryResponse is the jobs.query / getQueryResults response.
type queryResponse struct {
	JobComplete  bool        `json:"jobComplete"`
	Rows         []bqRow     `json:"rows"`
	PageToken    string      `json:"pageToken"`
	TotalRows    string      `json:"totalRows"`
	JobReference *jobRef     `json:"jobReference"`
	Error        *apiStatus  `json:"error"`
	Errors       []apiStatus `json:"errors"`
}

// run executes one statement and returns every row it produced, stopping
// as soon as more rows than the cap have arrived (the caller turns that
// into a truncation error rather than a smaller number).
func (q *Querier) run(ctx context.Context, stmt string, params []queryParam) ([]bqRow, error) {
	body := queryRequest{
		Query:           stmt,
		UseLegacySQL:    false,
		ParameterMode:   "NAMED",
		QueryParameters: params,
		TimeoutMs:       30000,
		MaxResults:      int64(q.maxRows) + 1,
		Location:        q.location,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gcplogging: encode query: %w", err)
	}
	resp, err := q.call(ctx, http.MethodPost, q.queriesURL(), raw)
	if err != nil {
		return nil, err
	}

	// A job that did not finish inline is polled by id until it does.
	for !resp.JobComplete {
		if resp.JobReference == nil || resp.JobReference.JobID == "" {
			return nil, fmt.Errorf("gcplogging: query is incomplete but the response carries no job id")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(q.pollInterval):
		}
		resp, err = q.call(ctx, http.MethodGet, q.resultsURL(resp.JobReference, ""), nil)
		if err != nil {
			return nil, err
		}
	}

	all := resp.Rows
	seen := map[string]bool{}
	for resp.PageToken != "" && len(all) <= q.maxRows {
		if seen[resp.PageToken] {
			return nil, fmt.Errorf("gcplogging: the API repeated page token %q — refusing to loop", resp.PageToken)
		}
		seen[resp.PageToken] = true
		if resp.JobReference == nil || resp.JobReference.JobID == "" {
			return nil, fmt.Errorf("gcplogging: a further page was offered but the response carries no job id")
		}
		resp, err = q.call(ctx, http.MethodGet, q.resultsURL(resp.JobReference, resp.PageToken), nil)
		if err != nil {
			return nil, err
		}
		if !resp.JobComplete {
			return nil, fmt.Errorf("gcplogging: the job went incomplete while paging results")
		}
		all = append(all, resp.Rows...)
	}
	return all, nil
}

func (q *Querier) queriesURL() string {
	return q.endpoint + "/bigquery/v2/projects/" + url.PathEscape(q.projectID) + "/queries"
}

func (q *Querier) resultsURL(ref *jobRef, pageToken string) string {
	v := url.Values{}
	v.Set("timeoutMs", "30000")
	v.Set("maxResults", strconv.Itoa(q.maxRows+1))
	loc := q.location
	if ref.Location != "" {
		loc = ref.Location
	}
	if loc != "" {
		v.Set("location", loc)
	}
	if pageToken != "" {
		v.Set("pageToken", pageToken)
	}
	return q.queriesURL() + "/" + url.PathEscape(ref.JobID) + "?" + v.Encode()
}

// call issues one request and decodes the response, turning any transport,
// HTTP or API-level failure into an error. Nothing here answers "no rows"
// on a failure: an unreadable backend must not look like an empty window.
func (q *Querier) call(ctx context.Context, method, u string, body []byte) (*queryResponse, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("gcplogging: request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if q.token != nil {
		tok, err := q.token()
		if err != nil {
			return nil, fmt.Errorf("gcplogging: bearer token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	httpResp, err := q.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcplogging: %s %s: %w", method, u, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	out, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("gcplogging: read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gcplogging: bigquery status %d: %s", httpResp.StatusCode, out)
	}
	var resp queryResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("gcplogging: decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("gcplogging: bigquery error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("gcplogging: bigquery error: %s", resp.Errors[0].Message)
	}
	return &resp, nil
}
