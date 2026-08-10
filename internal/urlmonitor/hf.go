// internal/urlmonitor/hf.go
//
// Hugging Face Spaces cha public URL (https://huggingface.co/spaces/owner/space
// kinva https://owner-space.hf.space) direct GET kartana bar problems yetat:
//
//   1. Private/gated spaces cha public URL barobar reachable naste — 401/403 yeto,
//      jari space actually "RUNNING" asla tari.
//   2. Sleeping space var GET kelyavar HF ek "loading / restarting" cha HTML page
//      200 OK sathi return karto — plain HTTP check tyala chukun "UP" mhanto,
//      pan prati actual API/model ready nasta.
//
// Yasathi HF cha official Spaces API (`/api/spaces/{owner}/{space}/runtime`)
// vaparaycha — ha nehmi (private space sathi HF_TOKEN dilyavar sudha) barobar
// runtime stage (RUNNING / SLEEPING / BUILDING / PAUSED / ...) deto.
package urlmonitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// hfSpaceHostRe matches the "owner-space.hf.space" runtime host format
var hfSpaceHostRe = regexp.MustCompile(`^([a-zA-Z0-9_]+)-(.+)\.hf\.space$`)

// isHuggingFaceURL — true jar dilela URL ek Hugging Face Space cha URL asel
// (either huggingface.co/spaces/... form, kinva *.hf.space runtime host form)
func isHuggingFaceURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	if host == "huggingface.co" && strings.HasPrefix(u.Path, "/spaces/") {
		return true
	}
	if strings.HasSuffix(host, ".hf.space") {
		return true
	}
	return false
}

// hfSpaceID — dilelya URL madhun "owner/space" identifier kadhto
func hfSpaceID(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Host)

	if host == "huggingface.co" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		// expected: ["spaces", "owner", "space", ...]
		if len(parts) >= 3 && parts[0] == "spaces" {
			return parts[1] + "/" + parts[2], nil
		}
		return "", fmt.Errorf("valid huggingface.co/spaces/<owner>/<space> URL nahi")
	}

	if strings.HasSuffix(host, ".hf.space") {
		if m := hfSpaceHostRe.FindStringSubmatch(host); m != nil {
			return m[1] + "/" + m[2], nil
		}
		return "", fmt.Errorf("hf.space host '%s' madhun space id parse hou shakla nahi", host)
	}

	return "", fmt.Errorf("hugging face URL nahi")
}

// hfRuntimeInfo — HF Spaces runtime API cha response (fakt garaj asleli fields)
type hfRuntimeInfo struct {
	Stage string `json:"stage"`
}

// pingHuggingFace — HF Spaces API var query karun space cha khara runtime
// status check karto, space cha swatah cha public URL var GET marnya evaji.
// token — per-monitor sathi save kelela HF token (private/gated space sathi).
// Rikama asel tar HF_TOKEN env var fallback mhanun vaparla jato.
func pingHuggingFace(rawURL, token string) PingResult {
	start := time.Now()
	result := PingResult{Timestamp: start}

	spaceID, err := hfSpaceID(rawURL)
	if err != nil {
		result.LatencyMs = time.Since(start).Milliseconds()
		result.IsUp = false
		result.Error = "invalid Hugging Face space URL: " + err.Error()
		return result
	}

	apiURL := "https://huggingface.co/api/spaces/" + spaceID + "/runtime"

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		result.LatencyMs = time.Since(start).Milliseconds()
		result.IsUp = false
		result.Error = err.Error()
		return result
	}
	// Private/gated spaces sathi token — pahile per-monitor token vapara,
	// nahi tar global HF_TOKEN env var fallback mhanun vapara.
	if token == "" {
		token = os.Getenv("HF_TOKEN")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	result.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		result.IsUp = false
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.IsUp = false
		result.Error = fmt.Sprintf("HF API %d — space private/gated aahe, add karte veli 'HF Token' field madhe token taka (kinva HF_TOKEN env var set kara)", resp.StatusCode)
		return result
	}
	if resp.StatusCode != http.StatusOK {
		result.IsUp = false
		result.Error = fmt.Sprintf("HF API returned %d", resp.StatusCode)
		return result
	}

	var info hfRuntimeInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		result.IsUp = false
		result.Error = "HF API response parse error: " + err.Error()
		return result
	}

	result.Stage = info.Stage

	switch info.Stage {
	case "RUNNING":
		result.IsUp = true
	case "":
		result.IsUp = false
		result.Error = "HF API made unknown runtime stage return"
	default:
		// SLEEPING, PAUSED, BUILDING, STARTING, STOPPED, RUNTIME_ERROR, etc.
		result.IsUp = false
		result.Error = "space stage: " + strings.ToLower(info.Stage)
	}

	return result
}
