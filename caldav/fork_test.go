package caldav

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	"io"
	"net/http/httptest"
	"runtime"
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

// RFC 4918 §9.1: a missing Depth means infinity; both are refused with propfind-finite-depth.
func TestForkPropfindInfiniteDepthRefused(t *testing.T) {
	h := &Handler{Backend: testBackend{calendars: []Calendar{{Path: "/user/calendars/cal/"}}}}
	for _, depth := range []string{"", "infinity"} {
		req := httptest.NewRequest("PROPFIND", "/user/calendars/", strings.NewReader(propfindCalendarProps))
		req.Header.Set("Content-Type", "application/xml")
		if depth != "" {
			req.Header.Set("Depth", depth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "propfind-finite-depth") {
			t.Errorf("Depth %q: %d %s", depth, w.Code, w.Body.String())
		}
	}
	if got := propfind(t, h, "/user/calendars/cal/", propfindCalendarProps); !strings.Contains(got, "multistatus") {
		t.Fatalf("Depth 0 must still answer: %s", got)
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

func TestForkRecurrenceMatchWideTimeSet(t *testing.T) {
	n := func(a, b int) string {
		var s []string
		for i := a; i < b; i++ {
			s = append(s, fmt.Sprint(i))
		}
		return strings.Join(s, ",")
	}
	co := recurringCal(t, "20260101T000000Z", "FREQ=YEARLY;BYMONTH="+n(1, 13)+";BYMONTHDAY="+n(1, 32)+
		";BYHOUR="+n(0, 24)+";BYMINUTE="+n(0, 60)+";BYSECOND="+n(0, 60))
	start := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ok, err := Match(rangeFilter(start, start.Add(time.Hour)), co)
	runtime.ReadMemStats(&after)
	if err != nil || !ok {
		t.Fatalf("got %v, %v; want conservative true", ok, err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("match allocated %d bytes; the rule was expanded", alloc)
	}
}

// rrule-go checks UNTIL only on a matching candidate: sub-daily BYSETPOS steps every period to year
// 9999, and an unreachable BYHOUR loops forever. Both must match conservatively, at once.
func TestForkRecurrenceMatchStallingRule(t *testing.T) {
	for _, tc := range []struct {
		dtstart, rule string
		want          bool
	}{
		{"20260101T090000Z", "FREQ=MINUTELY;BYSETPOS=2;UNTIL=20900101T000000Z", true},
		{"20260101T090000Z", "FREQ=HOURLY;INTERVAL=2;BYHOUR=10", true},
		{"20260101T090000Z", "FREQ=MINUTELY;INTERVAL=120;BYMINUTE=30", true},
		{"20260101T090000Z", "FREQ=HOURLY;INTERVAL=2;BYHOUR=11;COUNT=3", false}, // reachable: expanded, ends before the range
	} {
		co := recurringCal(t, tc.dtstart, tc.rule)
		start := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		done := make(chan bool, 1)
		go func() {
			ok, _ := Match(rangeFilter(start, start.Add(time.Hour)), co)
			done <- ok
		}()
		select {
		case ok := <-done:
			if ok != tc.want {
				t.Errorf("%s: got %v, want %v", tc.rule, ok, tc.want)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("%s: Match did not return within 500ms", tc.rule)
		}
	}
}

// RFC 4791 §9.9: (start < DTEND AND end > DTSTART); a zero-length event (start <= DTSTART AND end > DTSTART).
func TestForkTimeRangeBoundaries(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name, dtstart, dtend string
		want                 bool
	}{
		{"zero length at start", "20260101T100000Z", "20260101T100000Z", true},
		{"zero length at end", "20260101T120000Z", "20260101T120000Z", false},
		{"fills the range", "20260101T100000Z", "20260101T120000Z", true},
		{"ends at range end", "20260101T090000Z", "20260101T120000Z", true},
		{"ends at range start", "20260101T090000Z", "20260101T100000Z", false},
		{"starts at range end", "20260101T120000Z", "20260101T130000Z", false},
	} {
		src := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:b\r\nDTSTAMP:20261006T120000Z\r\nDTSTART:" + tc.dtstart + "\r\nDTEND:" + tc.dtend + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		cal, err := ical.NewDecoder(strings.NewReader(src)).Decode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := Match(rangeFilter(at(10), at(12)), &CalendarObject{Data: cal})
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
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

type collBackend struct {
	rawBackend
	created *Calendar
	update  *CalendarUpdate
}

func (b *collBackend) CreateCalendar(ctx context.Context, cal *Calendar) error {
	b.created = cal
	return nil
}

func (b *collBackend) UpdateCalendar(ctx context.Context, path string, u *CalendarUpdate) error {
	b.update = u
	return nil
}

func send(t *testing.T, h *Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestForkMkcalendar(t *testing.T) {
	b := &collBackend{}
	body := `<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:ic="http://apple.com/ns/ical/"><d:set><d:prop><d:displayname>Work</d:displayname><ic:calendar-color>#00ff00</ic:calendar-color></d:prop></d:set></c:mkcalendar>`
	w := send(t, &Handler{Backend: b}, "MKCALENDAR", "/user/calendars/work/", body)
	if w.Code != 201 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if b.created == nil || b.created.Name != "Work" || b.created.Color != "#00ff00" || b.created.Path != "/user/calendars/work/" {
		t.Fatalf("created %+v", b.created)
	}
}

func TestForkMkcalendarWrongPlace(t *testing.T) {
	w := send(t, &Handler{Backend: &collBackend{}}, "MKCALENDAR", "/user/", "")
	if w.Code != 403 {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestForkProppatch(t *testing.T) {
	b := &collBackend{}
	body := `<d:propertyupdate xmlns:d="DAV:" xmlns:ic="http://apple.com/ns/ical/"><d:set><d:prop><d:displayname>Home</d:displayname><ic:calendar-color>#123456</ic:calendar-color></d:prop></d:set></d:propertyupdate>`
	w := send(t, &Handler{Backend: b}, "PROPPATCH", "/user/calendars/cal/", body)
	if w.Code != 207 || !strings.Contains(w.Body.String(), "200") {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if b.update == nil || *b.update.Name != "Home" || *b.update.Color != "#123456" || b.update.Description != nil {
		t.Fatalf("update %+v", b.update)
	}
}

func TestForkProppatchUnknownPropIsAtomic(t *testing.T) {
	b := &collBackend{}
	body := `<d:propertyupdate xmlns:d="DAV:" xmlns:ic="http://apple.com/ns/ical/"><d:set><d:prop><d:displayname>Home</d:displayname><ic:calendar-order>3</ic:calendar-order></d:prop></d:set></d:propertyupdate>`
	w := send(t, &Handler{Backend: b}, "PROPPATCH", "/user/calendars/cal/", body)
	resp := w.Body.String()
	if w.Code != 207 || !strings.Contains(resp, "403") || !strings.Contains(resp, "424") {
		t.Fatalf("want 403 + 424 propstats, got %d %s", w.Code, resp)
	}
	if b.update != nil {
		t.Fatal("update applied despite a failed property")
	}
}

type countingBackend struct {
	rawBackend
	gets int
}

func (b *countingBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	b.gets++
	return b.rawBackend.GetCalendarObject(ctx, path, req)
}

// Each href costs a full object copy held until the response is written, so repeats are dropped.
func TestForkMultigetDeduplicatesHrefs(t *testing.T) {
	b := &countingBackend{}
	href := "<d:href>/user/calendars/cal/raw-1.ics</d:href>"
	body := `<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-data/></d:prop>` +
		strings.Repeat(href, 3) + `</c:calendar-multiget>`
	req := httptest.NewRequest("REPORT", "/user/calendars/cal/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	w := httptest.NewRecorder()
	(&Handler{Backend: b}).ServeHTTP(w, req)
	if w.Code != 207 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if b.gets != 1 || strings.Count(w.Body.String(), "raw-1.ics") != 1 {
		t.Fatalf("backend reads %d, body:\n%s", b.gets, w.Body.String())
	}
}

// Fork: MKCALENDAR passes supported-calendar-component-set to the backend.
func TestForkMkcalendarComponentSet(t *testing.T) {
	b := &collBackend{}
	body := `<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop><d:displayname>Tasks</d:displayname><c:supported-calendar-component-set><c:comp name="VTODO"/></c:supported-calendar-component-set></d:prop></d:set></c:mkcalendar>`
	w := send(t, &Handler{Backend: b}, "MKCALENDAR", "/user/calendars/tasks/", body)
	if w.Code != 201 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if b.created == nil || strings.Join(b.created.SupportedComponentSet, ",") != "VTODO" {
		t.Fatalf("created %+v", b.created)
	}
}

// Fork: RFC 4791 §9.9's VTODO table, over the range 10:00 to 12:00.
func TestForkToDoTimeRange(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC) }
	filter := CompFilter{Name: "VCALENDAR", Comps: []CompFilter{{Name: "VTODO", Start: at(10), End: at(12)}}}
	for _, tc := range []struct {
		name, props string
		want        bool
	}{
		{"start and duration overlapping", "DTSTART:20260101T090000Z\r\nDURATION:PT2H\r\n", true},
		{"start and duration ending at range start", "DTSTART:20260101T080000Z\r\nDURATION:PT2H\r\n", true},
		{"start and duration before", "DTSTART:20260101T070000Z\r\nDURATION:PT2H\r\n", false},
		{"start and due spanning", "DTSTART:20260101T090000Z\r\nDUE:20260101T130000Z\r\n", true},
		{"start and due before", "DTSTART:20260101T070000Z\r\nDUE:20260101T090000Z\r\n", false},
		{"start and due starting at range end", "DTSTART:20260101T120000Z\r\nDUE:20260101T130000Z\r\n", false},
		{"start at range start", "DTSTART:20260101T100000Z\r\n", true},
		{"start at range end", "DTSTART:20260101T120000Z\r\n", false},
		{"due at range end", "DUE:20260101T120000Z\r\n", true},
		{"due at range start", "DUE:20260101T100000Z\r\n", false},
		{"due after", "DUE:20260101T130000Z\r\n", false},
		{"created and completed spanning", "CREATED:20260101T080000Z\r\nCOMPLETED:20260101T110000Z\r\n", true},
		{"created and completed before", "CREATED:20260101T070000Z\r\nCOMPLETED:20260101T080000Z\r\n", false},
		{"completed at range end", "COMPLETED:20260101T120000Z\r\n", true},
		{"completed after", "COMPLETED:20260101T130000Z\r\n", false},
		{"created before", "CREATED:20260101T080000Z\r\n", true},
		{"created at range end", "CREATED:20260101T120000Z\r\n", false},
		{"undated", "SUMMARY:someday\r\n", true},
	} {
		src := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VTODO\r\nUID:t\r\nDTSTAMP:20261006T120000Z\r\n" + tc.props + "END:VTODO\r\nEND:VCALENDAR\r\n"
		cal, err := ical.NewDecoder(strings.NewReader(src)).Decode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := Match(filter, &CalendarObject{Data: cal})
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

type freeBusyBackend struct {
	testBackend
	periods  []FreeBusyPeriod
	gotPath  string
	gotStart time.Time
	gotEnd   time.Time
}

func (b *freeBusyBackend) QueryFreeBusy(ctx context.Context, path string, start, end time.Time) ([]FreeBusyPeriod, error) {
	b.gotPath, b.gotStart, b.gotEnd = path, start, end
	return b.periods, nil
}

func freeBusyReport(t *testing.T, h *Handler, path, timeRange string) *httptest.ResponseRecorder {
	t.Helper()
	body := `<C:free-busy-query xmlns:C="urn:ietf:params:xml:ns:caldav">` + timeRange + `</C:free-busy-query>`
	req := httptest.NewRequest("REPORT", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

const dayRange = `<C:time-range start="20261110T000000Z" end="20261111T000000Z"/>`

func at(hhmm string) time.Time {
	t, err := time.Parse("20060102T1504Z", "20261110T"+hhmm+"Z")
	if err != nil {
		panic(err)
	}
	return t
}

// RFC 4791 §7.10: one VFREEBUSY over the requested range, periods clipped, merged per FBTYPE.
func TestForkFreeBusyQuery(t *testing.T) {
	b := &freeBusyBackend{periods: []FreeBusyPeriod{
		{Start: at("1000"), End: at("1100")},
		{Start: at("0900"), End: at("1000")}, // touches the next: merged
		{Start: at("1030"), End: at("1045")}, // inside: merged away
		{Start: at("1300"), End: at("1400"), Type: FreeBusyTentative},
		{Start: at("1500"), End: at("1530"), Type: "X-UNKNOWN"}, // rendered as BUSY
		{Start: at("1600"), End: at("1700"), Type: FreeBusyUnavailable},
		{Start: at("0000").Add(-2 * time.Hour), End: at("0030")}, // clipped to the range start
		{Start: at("2300"), End: at("2300").Add(3 * time.Hour)},  // clipped to the range end
		{Start: at("1200"), End: at("1200")},                     // empty: dropped
	}}
	w := freeBusyReport(t, &Handler{Backend: b}, "/user/calendars/cal/", dayRange)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar" {
		t.Fatalf("status %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if b.gotPath != "/user/calendars/cal/" || !b.gotStart.Equal(at("0000")) || !b.gotEnd.Equal(at("0000").Add(24*time.Hour)) {
		t.Fatalf("backend got %q %v %v", b.gotPath, b.gotStart, b.gotEnd)
	}
	cal, err := ical.NewDecoder(w.Body).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Children) != 1 || cal.Children[0].Name != ical.CompFreeBusy {
		t.Fatalf("components %+v", cal.Children)
	}
	fb := cal.Children[0]
	if v := fb.Props.Get(ical.PropDateTimeStart).Value; v != "20261110T000000Z" {
		t.Errorf("DTSTART %s", v)
	}
	if v := fb.Props.Get(ical.PropDateTimeEnd).Value; v != "20261111T000000Z" {
		t.Errorf("DTEND %s", v)
	}
	var got []string
	for _, p := range fb.Props[ical.PropFreeBusy] {
		got = append(got, p.Params.Get(ical.ParamFreeBusyType)+" "+p.Value)
	}
	want := []string{
		" 20261110T000000Z/20261110T003000Z",
		" 20261110T090000Z/20261110T110000Z",
		" 20261110T150000Z/20261110T153000Z",
		" 20261110T230000Z/20261111T000000Z",
		"BUSY-TENTATIVE 20261110T130000Z/20261110T140000Z",
		"BUSY-UNAVAILABLE 20261110T160000Z/20261110T170000Z",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("FREEBUSY:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestForkFreeBusyQueryRefusals(t *testing.T) {
	h := &Handler{Backend: &freeBusyBackend{}}
	for name, c := range map[string]struct {
		path, timeRange string
		want            int
	}{
		"no time-range":        {"/user/calendars/cal/", "", 400},
		"no end":               {"/user/calendars/cal/", `<C:time-range start="20261110T000000Z"/>`, 400},
		"end before start":     {"/user/calendars/cal/", `<C:time-range start="20261111T000000Z" end="20261110T000000Z"/>`, 400},
		"home, not a calendar": {"/user/calendars/", dayRange, 403},
		"an object":            {"/user/calendars/cal/a.ics", dayRange, 403},
	} {
		if w := freeBusyReport(t, h, c.path, c.timeRange); w.Code != c.want {
			t.Errorf("%s: %d, want %d: %s", name, w.Code, c.want, w.Body.String())
		}
	}
	if w := freeBusyReport(t, &Handler{Backend: &rawBackend{}}, "/user/calendars/cal/", dayRange); w.Code != 403 {
		t.Errorf("backend without FreeBusyBackend: %d, want 403", w.Code)
	}
}

func TestForkFreeBusyQueryAdvertised(t *testing.T) {
	cal := Calendar{Path: "/user/calendars/cal/"}
	with := propfind(t, &Handler{Backend: &freeBusyBackend{testBackend: testBackend{calendars: []Calendar{cal}}}}, cal.Path, propfindCalendarProps)
	if !strings.Contains(with, "free-busy-query") {
		t.Errorf("FreeBusyBackend: supported-report-set lacks free-busy-query:\n%s", with)
	}
	without := propfind(t, &Handler{Backend: testBackend{calendars: []Calendar{cal}}}, cal.Path, propfindCalendarProps)
	if strings.Contains(without, "free-busy-query") {
		t.Errorf("plain backend advertises free-busy-query:\n%s", without)
	}
}
