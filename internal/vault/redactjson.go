package vault

import "encoding/json"

func redactJSON(r *Redactor, data any) any {
	b, err := json.Marshal(data)
	if err != nil {
		return data
	}
	s := string(b)
	scrubbed := r.RedactString(s)
	if scrubbed == s {
		return data
	}
	var generic any
	if err := json.Unmarshal([]byte(scrubbed), &generic); err != nil {
		return data
	}
	return generic
}
