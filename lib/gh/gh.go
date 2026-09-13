// Package gh talks to GitHub both ways: a minimal REST client (this file) for
// the handful of mutations koi performs (comment, close, reopen, milestone,
// label) plus the pre-mutation staleness check, and a GraphQL v4 client (the
// graphql*.go files) for the bulk reads everything else rides on.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/katbyte/go-kt/chttp"
	"github.com/katbyte/go-kt/clog"
)

type Repo struct {
	Owner string
	Name  string

	token      string
	httpClient *http.Client
}

func NewRepo(owner, name, token string) Repo {
	clog.Log.Debugf("new gh: %s/%s (%s)", owner, name, maskToken(token))
	return Repo{Owner: owner, Name: name, token: token, httpClient: chttp.NewHTTPClient("GitHub")}
}

func maskToken(t string) string {
	if len(t) < 8 {
		return "****"
	}
	return t[:4] + "****"
}

// do performs a request; callers interpret the status code.
func (r Repo) do(method, path string, payload any) (statusCode int, respBody []byte, err error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s%s", r.Owner, r.Name, path)

	var body io.Reader = http.NoBody
	if payload != nil {
		b, merr := json.Marshal(payload)
		if merr != nil {
			return 0, nil, fmt.Errorf("marshalling request body: %w", merr)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		return 0, nil, fmt.Errorf("building request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28") // canonical form; github matches case-insensitively
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response from %s: %w", url, err)
	}
	return resp.StatusCode, respBody, nil
}

// IssueState is the subset of issue/PR fields the staleness guard and the
// milestone sync need. The issues endpoint serves PRs too, milestone included.
type IssueState struct {
	Number    int       `json:"number"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
	Milestone *struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	} `json:"milestone"`
}

// GetIssue fetches the live state of an issue.
func (r Repo) GetIssue(number int) (*IssueState, error) {
	status, body, err := r.do(http.MethodGet, fmt.Sprintf("/issues/%d", number), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("getting issue #%d returned %d: %.200s", number, status, string(body))
	}

	var is IssueState
	if err := json.Unmarshal(body, &is); err != nil {
		return nil, fmt.Errorf("decoding issue #%d: %w", number, err)
	}
	return &is, nil
}

// CreateComment posts a comment on an issue.
func (r Repo) CreateComment(number int, text string) error {
	status, body, err := r.do(http.MethodPost, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": text})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("commenting on #%d returned %d: %.200s", number, status, string(body))
	}
	return nil
}

// CloseIssue closes an issue with a state reason ("not_planned", "completed", or "duplicate").
func (r Repo) CloseIssue(number int, stateReason string) error {
	status, body, err := r.do(http.MethodPatch, fmt.Sprintf("/issues/%d", number), map[string]string{"state": "closed", "state_reason": stateReason})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("closing #%d returned %d: %.200s", number, status, string(body))
	}
	return nil
}

// ReopenIssue reopens a closed issue.
func (r Repo) ReopenIssue(number int) error {
	status, body, err := r.do(http.MethodPatch, fmt.Sprintf("/issues/%d", number), map[string]string{"state": "open"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("reopening #%d returned %d: %.200s", number, status, string(body))
	}
	return nil
}

// SetMilestone assigns a milestone (by milestone number) to an issue.
func (r Repo) SetMilestone(issueNumber, milestoneNumber int) error {
	status, body, err := r.do(http.MethodPatch, fmt.Sprintf("/issues/%d", issueNumber), map[string]int{"milestone": milestoneNumber})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("setting milestone on #%d returned %d: %.200s", issueNumber, status, string(body))
	}
	return nil
}

// AddLabels adds labels to an issue (existing labels are kept).
func (r Repo) AddLabels(number int, labels []string) error {
	status, body, err := r.do(http.MethodPost, fmt.Sprintf("/issues/%d/labels", number), map[string][]string{"labels": labels})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("labelling #%d returned %d: %.200s", number, status, string(body))
	}
	return nil
}
