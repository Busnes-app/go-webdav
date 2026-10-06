package caldav

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func propfind(t *testing.T, h *Handler, path, body string) string {
	t.Helper()
	req := httptest.NewRequest("PROPFIND", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Depth", "0")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	data, _ := io.ReadAll(w.Result().Body)
	return string(data)
}

const propfindCalendarProps = `<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/" xmlns:ic="http://apple.com/ns/ical/">
  <d:prop><ic:calendar-color/><cs:getctag/><d:sync-token/><d:current-user-privilege-set/><d:supported-report-set/></d:prop>
</d:propfind>`

func TestForkCalendarProps(t *testing.T) {
	cal := Calendar{Path: "/user/calendars/cal/", Color: "#ff8800", CTag: "42", SyncToken: "urn:test:7"}
	h := &Handler{Backend: testBackend{calendars: []Calendar{cal}}}
	resp := propfind(t, h, cal.Path, propfindCalendarProps)
	for _, want := range []string{"#ff8800", ">42<", "urn:test:7", "sync-collection", "calendar-multiget", "write"} {
		if !strings.Contains(resp, want) {
			t.Errorf("missing %q in:\n%s", want, resp)
		}
	}
}

func TestForkReadOnlyCalendarHasNoWrite(t *testing.T) {
	cal := Calendar{Path: "/user/calendars/cal/", ReadOnly: true}
	h := &Handler{Backend: testBackend{calendars: []Calendar{cal}}}
	resp := propfind(t, h, cal.Path, propfindCalendarProps)
	if !strings.Contains(resp, "read") || strings.Contains(resp, "write") {
		t.Errorf("read-only calendar privileges wrong:\n%s", resp)
	}
	if strings.Contains(resp, "sync-collection") {
		t.Errorf("sync-collection advertised without a sync token:\n%s", resp)
	}
}
