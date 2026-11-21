package ghworkers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── GitHub REST API client (फक्त लागणारे calls) ─────────────────────────────

var httpClient = &http.Client{Timeout: 20 * time.Second}

type ghRun struct {
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	Event        string    `json:"event"`
	CreatedAt    time.Time `json:"created_at"`
	RunStartedAt time.Time `json:"run_started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func isActive(status string) bool {
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return true
	}
	return false
}

func ghCall(token, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "fsb-ghworkers")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, data, nil
}

// apiError — HTTP code सोबत, म्हणजे fatal (401/403/404/422) आणि temporary (5xx/rate-limit) वेगळे ओळखता येतात.
type apiError struct {
	Code int
	Msg  string
}

func (e *apiError) Error() string { return fmt.Sprintf("GitHub API %d: %s", e.Code, e.Msg) }

func apiErr(code int, data []byte) error {
	msg := strings.TrimSpace(string(data))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &apiError{Code: code, Msg: msg}
}

// listRuns — repository_dispatch ने सुरू झालेले अलीकडचे runs (नवीन पहिले).
func listRuns(token, owner, repo string) ([]ghRun, error) {
	code, data, err := ghCall(token, "GET",
		fmt.Sprintf("/repos/%s/%s/actions/runs?event=repository_dispatch&per_page=15", owner, repo), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, apiErr(code, data)
	}
	var out struct {
		Runs []ghRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}

func getRun(token, owner, repo string, id int64) (*ghRun, error) {
	code, data, err := ghCall(token, "GET", fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, repo, id), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, apiErr(code, data)
	}
	var r ghRun
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func dispatch(token, owner, repo, eventType string) error {
	code, data, err := ghCall(token, "POST", fmt.Sprintf("/repos/%s/%s/dispatches", owner, repo),
		map[string]string{"event_type": eventType})
	if err != nil {
		return err
	}
	if code != http.StatusNoContent {
		return apiErr(code, data)
	}
	return nil
}

func cancelRun(token, owner, repo string, id int64, force bool) error {
	suffix := "cancel"
	if force {
		suffix = "force-cancel"
	}
	code, data, err := ghCall(token, "POST", fmt.Sprintf("/repos/%s/%s/actions/runs/%d/%s", owner, repo, id, suffix), nil)
	if err != nil {
		return err
	}
	// 202 = accepted, 409 = run आधीच संपलाय / cancel होऊ शकत नाही → दोन्ही ठीक.
	if code == http.StatusAccepted || code == http.StatusConflict {
		return nil
	}
	return apiErr(code, data)
}

// verifyRepo — token ने repo दिसतो का + push permission (repository_dispatch साठी) + Actions read access.
func verifyRepo(token, owner, repo string) error {
	code, data, err := ghCall(token, "GET", fmt.Sprintf("/repos/%s/%s", owner, repo), nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return apiErr(code, data)
	}
	var r struct {
		Permissions *struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal(data, &r)
	if r.Permissions != nil && !r.Permissions.Push {
		return fmt.Errorf("token ला repo वर write access नाही (repository_dispatch साठी Contents: write / repo scope लागतो)")
	}
	code, data, err = ghCall(token, "GET", fmt.Sprintf("/repos/%s/%s/actions/runs?per_page=1", owner, repo), nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("Actions runs वाचता येत नाहीत — token ला Actions: read/write (किंवा classic 'workflow' scope) द्या. %v", apiErr(code, data))
	}
	return nil
}
