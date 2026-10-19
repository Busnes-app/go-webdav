package caldav

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-ical"
)

type schedBackend struct {
	testBackend
	on      bool
	gotReq  *ical.Calendar
	gotTag  string
	putTag  string
	objects map[string]*CalendarObject
}

var testScheduling = &Scheduling{
	CalendarUserAddresses: []string{"mailto:alice@example.com"},
	InboxPath:             "/user/calendars/_inbox/",
	OutboxPath:            "/user/calendars/_outbox/",
	DefaultCalendarPath:   "/user/calendars/cal/",
}

func (b *schedBackend) Scheduling(ctx context.Context) (*Scheduling, error) {
	if !b.on {
		return nil, nil
	}
	return testScheduling, nil
}

func (b *schedBackend) ScheduleFreeBusy(ctx context.Context, req *ical.Calendar) ([]ScheduleResponse, error) {
	b.gotReq = req
	fb := ical.NewCalendar()
	fb.Props.SetText(ical.PropVersion, "2.0")
	fb.Props.SetText(ical.PropProductID, "-//t//EN")
	fb.Props.SetText(ical.PropMethod, "REPLY")
	c := ical.NewComponent(ical.CompFreeBusy)
	c.Props.SetText(ical.PropUID, "fb")
	c.Props.SetText(ical.PropDateTimeStamp, "20261101T000000Z")
	fb.Children = append(fb.Children, c)
	return []ScheduleResponse{
		{Recipient: "mailto:bob@example.com", Status: "2.0;Success", Calendar: fb},
		{Recipient: "mailto:carol@example.net", Status: "3.7;Invalid calendar user", Calendar: fb},
	}, nil
}

func (b *schedBackend) GetCalendarObject(ctx context.Context, path string, req *CalendarCompRequest) (*CalendarObject, error) {
	return &CalendarObject{Path: path, ETag: "e1", ScheduleTag: b.putTag, Raw: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")}, nil
}

func (b *schedBackend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *PutCalendarObjectOptions) (*CalendarObject, error) {
	b.gotTag = opts.IfScheduleTagMatch
	return &CalendarObject{Path: path, ETag: "e2", ScheduleTag: b.putTag}, nil
}

func schedHandler(on bool) (*Handler, *schedBackend) {
	b := &schedBackend{on: on, testBackend: testBackend{calendars: []Calendar{{Path: "/user/calendars/cal/"}}}}
	return &Handler{Backend: b}, b
}

const principalProps = `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop>
<c:calendar-user-address-set/><c:schedule-inbox-URL/><c:schedule-outbox-URL/></d:prop></d:propfind>`

const freeBusyRequest = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\nMETHOD:REQUEST\r\nBEGIN:VFREEBUSY\r\nUID:q1\r\nDTSTAMP:20261101T000000Z\r\n" +
	"DTSTART:20261110T000000Z\r\nDTEND:20261111T000000Z\r\nORGANIZER:mailto:alice@example.com\r\nATTENDEE:mailto:bob@example.com\r\n" +
	"ATTENDEE:mailto:carol@example.net\r\nEND:VFREEBUSY\r\nEND:VCALENDAR\r\n"

func do(t *testing.T, h *Handler, method, path, ct, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Off (no SchedulingBackend, or one returning nil), nothing about scheduling appears, and a
// POST is answered exactly as before.
func TestForkSchedulingOffIsInvisible(t *testing.T) {
	plain := &Handler{Backend: testBackend{calendars: []Calendar{{Path: "/user/calendars/cal/"}}}}
	off, _ := schedHandler(false)
	for name, h := range map[string]*Handler{"no backend": plain, "backend off": off} {
		if dav := do(t, h, "OPTIONS", "/user/calendars/cal/", "", "", nil).Header().Get("DAV"); strings.Contains(dav, "auto-schedule") {
			t.Errorf("%s: DAV %q", name, dav)
		}
		body := propfind(t, h, "/user/", principalProps)
		if !strings.Contains(body, "404 Not Found") || strings.Contains(body, "_inbox") {
			t.Errorf("%s: principal answers scheduling props:\n%s", name, body)
		}
		if got, want := do(t, h, "POST", "/user/calendars/_outbox/", ical.MIMEType, freeBusyRequest, nil), do(t, plain, "POST", "/user/calendars/_outbox/", ical.MIMEType, freeBusyRequest, nil); got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("%s: POST %d %q, want the unscheduled answer %d %q", name, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
	}
}

func TestForkSchedulingAdvertised(t *testing.T) {
	h, _ := schedHandler(true)
	if dav := do(t, h, "OPTIONS", "/user/calendars/cal/", "", "", nil).Header().Get("DAV"); !strings.Contains(dav, "calendar-auto-schedule") {
		t.Errorf("DAV %q", dav)
	}
	body := propfind(t, h, "/user/", principalProps)
	for _, want := range []string{"mailto:alice@example.com", "/user/calendars/_inbox/", "/user/calendars/_outbox/"} {
		if !strings.Contains(body, want) {
			t.Errorf("principal lacks %q:\n%s", want, body)
		}
	}
	inbox := propfind(t, h, "/user/calendars/_inbox/", `<d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`)
	for _, want := range []string{"schedule-inbox", "schedule-default-calendar-URL", "/user/calendars/cal/", "schedule-deliver"} {
		if !strings.Contains(inbox, want) {
			t.Errorf("inbox lacks %q:\n%s", want, inbox)
		}
	}
	outbox := propfind(t, h, "/user/calendars/_outbox/", `<d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`)
	for _, want := range []string{"schedule-outbox", "schedule-send"} {
		if !strings.Contains(outbox, want) {
			t.Errorf("outbox lacks %q:\n%s", want, outbox)
		}
	}
	if allow := do(t, h, "OPTIONS", "/user/calendars/_outbox/", "", "", nil).Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("outbox Allow %q", allow)
	}
	cal := propfind(t, h, "/user/calendars/cal/", `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:schedule-calendar-transp/></d:prop></d:propfind>`)
	if !strings.Contains(cal, "opaque") {
		t.Errorf("calendar transp:\n%s", cal)
	}
}

func TestForkOutboxFreeBusy(t *testing.T) {
	h, b := schedHandler(true)
	w := do(t, h, "POST", "/user/calendars/_outbox/", ical.MIMEType, freeBusyRequest, nil)
	body, _ := io.ReadAll(w.Body)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("POST %d %s", w.Code, body)
	}
	s := string(body)
	for _, want := range []string{"schedule-response", "mailto:bob@example.com", "2.0;Success", "BEGIN:VFREEBUSY", "mailto:carol@example.net", "3.7;Invalid calendar user"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "BEGIN:VFREEBUSY") != 1 {
		t.Errorf("calendar-data sent for a failed recipient:\n%s", s)
	}
	if b.gotReq == nil {
		t.Fatal("backend not asked")
	}
	for name, req := range map[string]string{
		"not REQUEST": strings.Replace(freeBusyRequest, "METHOD:REQUEST", "METHOD:PUBLISH", 1),
		"no ATTENDEE": strings.Replace(strings.Replace(freeBusyRequest, "ATTENDEE:mailto:bob@example.com\r\n", "", 1), "ATTENDEE:mailto:carol@example.net\r\n", "", 1),
		"a VEVENT":    strings.Replace(freeBusyRequest, "VFREEBUSY", "VEVENT", 2),
		"no DTEND":    strings.Replace(freeBusyRequest, "DTEND:20261111T000000Z\r\n", "", 1),
	} {
		if w := do(t, h, "POST", "/user/calendars/_outbox/", ical.MIMEType, req, nil); w.Code != 409 || !strings.Contains(w.Body.String(), "valid-scheduling-message") {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := do(t, h, "POST", "/user/calendars/_outbox/", "text/plain", freeBusyRequest, nil); w.Code != 400 {
		t.Errorf("wrong content type: %d", w.Code)
	}
	if w := do(t, h, "POST", "/user/calendars/_outbox/", ical.MIMEType, "garbage", nil); w.Code != 409 || !strings.Contains(w.Body.String(), "valid-calendar-data") {
		t.Errorf("garbage: %d %s", w.Code, w.Body.String())
	}
}

func TestForkScheduleTag(t *testing.T) {
	h, b := schedHandler(true)
	b.putTag = "st1"
	if tag := do(t, h, "GET", "/user/calendars/cal/a.ics", "", "", nil).Header().Get("Schedule-Tag"); tag != `"st1"` {
		t.Errorf("GET Schedule-Tag %q", tag)
	}
	w := do(t, h, "PUT", "/user/calendars/cal/a.ics", ical.MIMEType, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261101T000000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n",
		map[string]string{"If-Schedule-Tag-Match": `"st0"`})
	if w.Header().Get("Schedule-Tag") != `"st1"` || b.gotTag != `"st0"` {
		t.Errorf("PUT %d Schedule-Tag %q, backend got %q", w.Code, w.Header().Get("Schedule-Tag"), b.gotTag)
	}
	b.putTag = ""
	if tag := do(t, h, "GET", "/user/calendars/cal/a.ics", "", "", nil).Header().Get("Schedule-Tag"); tag != "" {
		t.Errorf("no tag set, header %q", tag)
	}
}
