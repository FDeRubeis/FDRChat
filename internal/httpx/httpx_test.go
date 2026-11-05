package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type payload struct {
	Name string `json:"name"`
}

// jsonOfSize returns a valid JSON object that is exactly size bytes long.
func jsonOfSize(t *testing.T, size int) string {
	t.Helper()
	const overhead = len(`{"name":""}`)
	if size < overhead {
		t.Fatalf("size %d too small", size)
	}
	s := `{"name":"` + strings.Repeat("a", size-overhead) + `"}`
	if len(s) != size {
		t.Fatalf("len = %d, want %d", len(s), size)
	}
	return s
}

func decode(body, contentType string) (payload, *Error) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	var p payload
	err := DecodeJSON(httptest.NewRecorder(), req, &p)
	return p, err
}

func TestDecodeJSONAccepts(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        payload
	}{
		{"plain object", "application/json", `{"name":"alice"}`, payload{"alice"}},
		{"trailing newline", "application/json", "{\"name\":\"alice\"}\n", payload{"alice"}},
		{"surrounding whitespace", "application/json", " \t\r\n{\"name\":\"alice\"} \r\n", payload{"alice"}},
		{"missing field leaves zero value", "application/json", `{}`, payload{}},
		// Documents current behaviour: the Content-Type header is not checked.
		{"any content type", "text/plain", `{"name":"alice"}`, payload{"alice"}},
		{"no content type", "", `{"name":"alice"}`, payload{"alice"}},
		// Documents current behaviour: null is a no-op for a struct, so
		// callers must validate the result themselves.
		{"null leaves zero value", "application/json", `null`, payload{}},
		{"exactly the size limit", "application/json", jsonOfSize(t, maxBodyBytes), payload{strings.Repeat("a", maxBodyBytes-len(`{"name":""}`))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decode(tt.body, tt.contentType)
			if err != nil {
				t.Fatalf("unexpected error: %+v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeJSONRejects(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{"empty body", ``, http.StatusBadRequest, "invalid JSON body"},
		{"whitespace only", " \n", http.StatusBadRequest, "invalid JSON body"},
		{"truncated", `{"name":`, http.StatusBadRequest, "invalid JSON body"},
		{"not json", `hello`, http.StatusBadRequest, "invalid JSON body"},
		{"array instead of object", `["alice"]`, http.StatusBadRequest, "invalid JSON body"},
		{"wrong field type", `{"name":123}`, http.StatusBadRequest, "invalid JSON body"},
		{"unknown field", `{"name":"alice","admin":true}`, http.StatusBadRequest, "invalid JSON body"},
		{"second object", `{"name":"alice"}{"name":"bob"}`, http.StatusBadRequest, "body must contain a single JSON object"},
		{"trailing garbage", `{"name":"alice"} x`, http.StatusBadRequest, "body must contain a single JSON object"},
		{"one byte over the size limit", jsonOfSize(t, maxBodyBytes+1), http.StatusRequestEntityTooLarge, "request body too large"},
		{"far over the size limit", `{"name":"` + strings.Repeat("a", 4*maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "request body too large"},
		// Documents current behaviour: when the object itself fits and only
		// the trailing bytes push the body over the limit, the client gets
		// 400 rather than 413.
		{
			"valid object followed by oversized padding",
			`{"name":"alice"}` + strings.Repeat(" ", 2*maxBodyBytes),
			http.StatusBadRequest,
			"body must contain a single JSON object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decode(tt.body, "application/json")
			if err == nil {
				t.Fatal("expected an error")
			}
			if err.Status != tt.wantStatus || err.Message != tt.wantMsg {
				t.Fatalf("got %d %q, want %d %q", err.Status, err.Message, tt.wantStatus, tt.wantMsg)
			}
		})
	}
}

func TestDecodeJSONErrorDoesNotEchoInput(t *testing.T) {
	_, err := decode(`{"name":"super-secret-value","admin":true}`, "application/json")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Message, "super-secret-value") || strings.Contains(err.Message, "admin") {
		t.Fatalf("message echoes client input: %q", err.Message)
	}
}

func TestErrorImplementsErrorInterface(t *testing.T) {
	var err error = &Error{Status: http.StatusTeapot, Message: "short and stout"}
	if err.Error() != "short and stout" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, payload{Name: "alice"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got payload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v (%q)", err, rec.Body)
	}
	if got.Name != "alice" {
		t.Fatalf("got %+v", got)
	}
}

func TestWriteJSONEscapesHTML(t *testing.T) {
	// Documents current behaviour of json.Encoder defaults: <, > and & are
	// escaped, which keeps API responses safe if ever sniffed as HTML.
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, payload{Name: "<script>&"})

	if strings.Contains(rec.Body.String(), "<script>") {
		t.Fatalf("body contains raw HTML: %q", rec.Body)
	}
	var got payload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Name != "<script>&" {
		t.Fatalf("round trip failed: %+v, %v", got, err)
	}
}

func TestWriteJSONNilBodyIsJSONNull(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, nil)

	if strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("body = %q, want null", rec.Body)
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnprocessableEntity, "name is required")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v (%q)", err, rec.Body)
	}
	if len(got) != 1 || got["error"] != "name is required" {
		t.Fatalf("got %v, want only {error: name is required}", got)
	}
}
