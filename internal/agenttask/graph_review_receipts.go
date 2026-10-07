package agenttask

import (
	"encoding/json"
	"strings"
	"time"
)

// The qualitative gate needs execution receipts, not repeated page/navigation
// bodies. Full saved sources remain available to the independent grounding gate.
// Preserve all non-browser outputs and failed browser attempts verbatim.
func graphReviewReceipts(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`[]`), nil
	}
	var traces []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &traces); err != nil {
		return nil, err
	}
	for _, trace := range traces {
		var name string
		_ = json.Unmarshal(trace["name"], &name)
		if name == "" {
			_ = json.Unmarshal(trace["tool"], &name)
		}
		if name != "browser_fetch" && name != "browser_search" {
			continue
		}
		failed := false
		for _, key := range []string{"error", "timed_out"} {
			value := strings.TrimSpace(string(trace[key]))
			if value != "" && value != "false" && value != "null" && value != `""` {
				failed = true
			}
		}
		if failed {
			continue
		}
		var output string
		if err := json.Unmarshal(trace["output"], &output); err != nil {
			continue
		}
		receipt := browserReviewMetadata(output)
		if len(receipt) == 0 {
			continue
		}
		receipt["content_omitted"] = json.RawMessage(`true`)
		compact, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		// Keep the string-valued output trace contract for downstream consumers.
		trace["output"], err = json.Marshal(string(compact))
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(traces)
}

// Traces may be truncated in the middle of links or text. Decode only complete
// top-level values preceding truncation; never manufacture a missing receipt.
func browserReviewMetadata(output string) map[string]json.RawMessage {
	receipt := map[string]json.RawMessage{}
	decoder := json.NewDecoder(strings.NewReader(output))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return receipt
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		key, ok := token.(string)
		if !ok {
			break
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			break
		}
		switch key {
		case "observed_at":
			var stamp string
			if json.Unmarshal(value, &stamp) == nil {
				if _, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
					receipt[key] = value
				}
			}
		case "url", "final_url", "requested_url", "engine_used", "cached", "status_code", "error":
			receipt[key] = value
		}
	}
	return receipt
}
