package caldav

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-ical"
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

const rawEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:raw-1\r\nDTSTAMP:20261006T120000Z\r\nDTSTART:20261007T090000Z\r\nX-APPLE-ODD;X-P=1:keep  this\r\nSUMMARY:Raw\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

type rawBackend struct {
	testBackend
	putRaw []byte
}

func (b *rawBackend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error) {
	b.putRaw = opts.Raw
	return &CalendarObject{Path: path, ETag: "e1"}, nil
}

func (b *rawBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	cal, err := ical.NewDecoder(bytes.NewReader([]byte(rawEvent))).Decode()
	if err != nil {
		return nil, err
	}
	return &CalendarObject{Path: path, ETag: "e1", Data: cal, Raw: []byte(rawEvent)}, nil
}

func TestForkPutPassesRawBytes(t *testing.T) {
	b := &rawBackend{}
	h := &Handler{Backend: b, MaxResourceSize: 1 << 20}
	req := httptest.NewRequest("PUT", "/user/calendars/cal/raw-1.ics", strings.NewReader(rawEvent))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if string(b.putRaw) != rawEvent {
		t.Fatalf("raw bytes changed:\n%q", b.putRaw)
	}
}

func TestForkGetServesRawBytes(t *testing.T) {
	h := &Handler{Backend: &rawBackend{}}
	req := httptest.NewRequest("GET", "/user/calendars/cal/raw-1.ics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Body.String() != rawEvent {
		t.Fatalf("GET body differs:\n%q", w.Body.String())
	}
	if w.Header().Get("Content-Length") != fmt.Sprint(len(rawEvent)) {
		t.Fatalf("Content-Length %q", w.Header().Get("Content-Length"))
	}
}

func TestForkPutOverLimit(t *testing.T) {
	h := &Handler{Backend: &rawBackend{}, MaxResourceSize: 64}
	req := httptest.NewRequest("PUT", "/user/calendars/cal/raw-1.ics", strings.NewReader(rawEvent))
	req.Header.Set("Content-Type", "text/calendar")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "max-resource-size") {
		t.Fatalf("want max-resource-size precondition, got %d %s", w.Code, w.Body.String())
	}
}

func recurringCal(t *testing.T, dtstart, rrule string) *CalendarObject {
	t.Helper()
	src := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:r\r\nDTSTAMP:20261006T120000Z\r\nDTSTART:" + dtstart + "\r\nRRULE:" + rrule + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := ical.NewDecoder(strings.NewReader(src)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return &CalendarObject{Data: cal}
}

func rangeFilter(start, end time.Time) CompFilter {
	return CompFilter{Name: "VCALENDAR", Comps: []CompFilter{{Name: "VEVENT", Start: start, End: end}}}
}

func TestForkRecurrenceMatchBounded(t *testing.T) {
	co := recurringCal(t, "19700101T000000Z", "FREQ=SECONDLY")
	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	begin := time.Now()
	ok, err := Match(rangeFilter(start, start.Add(time.Hour)), co)
	if err != nil || !ok {
		t.Fatalf("got %v, %v; want conservative true", ok, err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("match took %v", d)
	}
}

func TestForkRecurrenceMatchSemantics(t *testing.T) {
	co := recurringCal(t, "20260105T090000Z", "FREQ=WEEKLY;COUNT=3") // Mondays: 5, 12, 19 Jan
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name       string
		start, end time.Time
		want       bool
	}{
		{"inside", day(11), day(13), true},
		{"between", day(6), day(11), false},
		{"before", day(1), day(4), false},
		{"after count", day(20), day(31), false},
		{"unbounded end", day(13), time.Time{}, true},
	} {
		got, err := Match(rangeFilter(tc.start, tc.end), co)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

type syncBackend struct {
	rawBackend
	gotToken string
}

func (b *syncBackend) SyncCalendar(ctx context.Context, path, token string) (*SyncResult, error) {
	b.gotToken = token
	if token == "bad" {
		return nil, ErrInvalidSyncToken
	}
	obj, _ := b.GetCalendarObject(ctx, path+"raw-1.ics", nil)
	return &SyncResult{SyncToken: "urn:test:9", Updated: []CalendarObject{*obj}, Deleted: []string{path + "gone.ics"}}, nil
}

func syncReport(t *testing.T, h *Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := `<d:sync-collection xmlns:d="DAV:"><d:sync-token>` + token + `</d:sync-token><d:sync-level>1</d:sync-level><d:prop><d:getetag/></d:prop></d:sync-collection>`
	req := httptest.NewRequest("REPORT", "/user/calendars/cal/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestForkSyncCollection(t *testing.T) {
	b := &syncBackend{}
	w := syncReport(t, &Handler{Backend: b}, "urn:test:7")
	if w.Code != 207 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	resp := w.Body.String()
	for _, want := range []string{"urn:test:9", "raw-1.ics", "e1", "gone.ics", "404"} {
		if !strings.Contains(resp, want) {
			t.Errorf("missing %q in:\n%s", want, resp)
		}
	}
	if b.gotToken != "urn:test:7" {
		t.Errorf("token passed %q", b.gotToken)
	}
}

func TestForkSyncCollectionInvalidToken(t *testing.T) {
	w := syncReport(t, &Handler{Backend: &syncBackend{}}, "bad")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "valid-sync-token") {
		t.Fatalf("want 403 valid-sync-token, got %d %s", w.Code, w.Body.String())
	}
}

func TestForkSyncCollectionUnsupported(t *testing.T) {
	w := syncReport(t, &Handler{Backend: &rawBackend{}}, "")
	if w.Code != 403 {
		t.Fatalf("want 403 for backend without SyncBackend, got %d", w.Code)
	}
}
