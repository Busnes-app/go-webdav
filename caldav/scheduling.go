package caldav

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/internal"
)

// Fork: SchedulingBackend is implemented by backends that schedule on the server (RFC 6638).
// While Scheduling returns nil, nothing about scheduling is advertised or served, so a backend
// can switch it off at run time and clients see exactly an unscheduled server.
type SchedulingBackend interface {
	Scheduling(ctx context.Context) (*Scheduling, error)
	// ScheduleFreeBusy answers an outbox VFREEBUSY request (RFC 6638 §5), one response per
	// ATTENDEE. The handler has checked the request's shape; the backend decides who may ask.
	ScheduleFreeBusy(ctx context.Context, request *ical.Calendar) ([]ScheduleResponse, error)
}

// Scheduling is the current user's scheduling collections and addresses.
type Scheduling struct {
	CalendarUserAddresses []string // "mailto:" URIs
	InboxPath             string
	OutboxPath            string
	DefaultCalendarPath   string
}

// ScheduleResponse is one recipient's answer in a schedule-response.
type ScheduleResponse struct {
	Recipient string
	Status    string         // iTIP REQUEST-STATUS, such as "2.0;Success" or "3.7;Invalid calendar user"
	Calendar  *ical.Calendar // the VFREEBUSY reply; nil unless Status is 2.x
}

var (
	calendarUserAddressSetName     = xml.Name{namespace, "calendar-user-address-set"}
	scheduleInboxURLName           = xml.Name{namespace, "schedule-inbox-URL"}
	scheduleOutboxURLName          = xml.Name{namespace, "schedule-outbox-URL"}
	scheduleDefaultCalendarURLName = xml.Name{namespace, "schedule-default-calendar-URL"}
	scheduleCalendarTranspName     = xml.Name{namespace, "schedule-calendar-transp"}
	scheduleTagName                = xml.Name{namespace, "schedule-tag"}
	scheduleInboxName              = xml.Name{namespace, "schedule-inbox"}
	scheduleOutboxName             = xml.Name{namespace, "schedule-outbox"}
)

type hrefSet struct {
	XMLName xml.Name
	Hrefs   []string `xml:"DAV: href"`
}

type textProp struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type rawProp struct {
	XMLName xml.Name
	Raw     []internal.RawXMLValue `xml:",any"`
}

type privilegeSet struct {
	XMLName   xml.Name  `xml:"DAV: current-user-privilege-set"`
	Privilege []rawProp `xml:"DAV: privilege"`
}

func privileges(names ...xml.Name) *privilegeSet {
	set := &privilegeSet{}
	for _, n := range names {
		set.Privilege = append(set.Privilege, rawProp{XMLName: xml.Name{"DAV:", "privilege"}, Raw: []internal.RawXMLValue{*internal.NewRawXMLElement(n, nil, nil)}})
	}
	return set
}

func (b *backend) scheduling(ctx context.Context) (*Scheduling, error) {
	sb, ok := b.Backend.(SchedulingBackend)
	if !ok {
		return nil, nil
	}
	return sb.Scheduling(ctx)
}

func (b *backend) addPrincipalScheduling(props map[xml.Name]internal.PropFindFunc, s *Scheduling) {
	props[calendarUserAddressSetName] = internal.PropFindValue(&hrefSet{XMLName: calendarUserAddressSetName, Hrefs: s.CalendarUserAddresses})
	props[scheduleInboxURLName] = internal.PropFindValue(&hrefSet{XMLName: scheduleInboxURLName, Hrefs: []string{s.InboxPath}})
	props[scheduleOutboxURLName] = internal.PropFindValue(&hrefSet{XMLName: scheduleOutboxURLName, Hrefs: []string{s.OutboxPath}})
}

func calendarTransp(transparent bool) *rawProp {
	v := "opaque"
	if transparent {
		v = "transparent"
	}
	return &rawProp{XMLName: scheduleCalendarTranspName, Raw: []internal.RawXMLValue{*internal.NewRawXMLElement(xml.Name{namespace, v}, nil, nil)}}
}

func scheduleTag(tag string) *textProp {
	return &textProp{XMLName: scheduleTagName, Value: internal.ETag(tag).String()}
}

func (b *backend) propFindScheduleCollection(ctx context.Context, propfind *internal.PropFind, s *Scheduling, path string) (*internal.Response, error) {
	principal, err := b.Backend.CurrentUserPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	props := map[xml.Name]internal.PropFindFunc{
		internal.CurrentUserPrincipalName: internal.PropFindValue(&internal.CurrentUserPrincipal{Href: internal.Href{Path: principal}}),
	}
	if path == s.InboxPath {
		props[internal.ResourceTypeName] = internal.PropFindValue(internal.NewResourceType(internal.CollectionName, scheduleInboxName))
		props[internal.CurrentUserPrivilegeSetName] = internal.PropFindValue(privileges(xml.Name{"DAV:", "read"}, xml.Name{namespace, "schedule-deliver"}))
		props[scheduleDefaultCalendarURLName] = internal.PropFindValue(&hrefSet{XMLName: scheduleDefaultCalendarURLName, Hrefs: []string{s.DefaultCalendarPath}})
	} else {
		props[internal.ResourceTypeName] = internal.PropFindValue(internal.NewResourceType(internal.CollectionName, scheduleOutboxName))
		props[internal.CurrentUserPrivilegeSetName] = internal.PropFindValue(privileges(xml.Name{"DAV:", "read"}, xml.Name{namespace, "schedule-send"}))
	}
	return internal.NewPropFindResponse(path, propfind, props)
}

// isScheduleCollection reports whether path is the user's inbox or outbox while scheduling is on.
func (b *backend) isScheduleCollection(ctx context.Context, path string) (*Scheduling, bool, error) {
	s, err := b.scheduling(ctx)
	if err != nil || s == nil {
		return nil, false, err
	}
	return s, path == s.InboxPath || path == s.OutboxPath, nil
}

type scheduleResponseXML struct {
	XMLName  xml.Name               `xml:"urn:ietf:params:xml:ns:caldav schedule-response"`
	Response []scheduleResponseItem `xml:"urn:ietf:params:xml:ns:caldav response"`
}

type scheduleResponseItem struct {
	Recipient     hrefSet `xml:"urn:ietf:params:xml:ns:caldav recipient"`
	RequestStatus string  `xml:"urn:ietf:params:xml:ns:caldav request-status"`
	CalendarData  string  `xml:"urn:ietf:params:xml:ns:caldav calendar-data,omitempty"`
}

// maxOutboxBody caps an outbox POST when the handler sets no MaxResourceSize: a free/busy
// request is a few hundred bytes per attendee.
const maxOutboxBody = 1 << 20

// handleOutboxPost answers a VFREEBUSY request POSTed to the outbox (RFC 6638 §5).
func (h *Handler) handleOutboxPost(w http.ResponseWriter, r *http.Request, s *Scheduling) error {
	sb := h.Backend.(SchedulingBackend)
	if t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || t != ical.MIMEType {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: outbox POST needs text/calendar")
	}
	limit := h.MaxResourceSize
	if limit <= 0 {
		limit = maxOutboxBody
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return internal.HTTPErrorf(http.StatusBadRequest, "caldav: failed to read body: %v", err)
	}
	if int64(len(raw)) > limit {
		return NewPreconditionError(PreconditionMaxResourceSize)
	}
	cal, err := ical.NewDecoder(bytes.NewReader(raw)).Decode()
	if err != nil {
		return NewPreconditionError(PreconditionValidCalendarData)
	}
	if !validFreeBusyRequest(cal) {
		return NewPreconditionError(PreconditionValidSchedulingMessage)
	}
	resps, err := sb.ScheduleFreeBusy(r.Context(), cal)
	if err != nil {
		return err
	}
	out := scheduleResponseXML{}
	for _, resp := range resps {
		item := scheduleResponseItem{Recipient: hrefSet{XMLName: xml.Name{namespace, "recipient"}, Hrefs: []string{resp.Recipient}}, RequestStatus: resp.Status}
		if resp.Calendar != nil && strings.HasPrefix(resp.Status, "2.") {
			var buf bytes.Buffer
			if err := ical.NewEncoder(&buf).Encode(resp.Calendar); err != nil {
				return err
			}
			item.CalendarData = buf.String()
		}
		out.Response = append(out.Response, item)
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return err
	}
	return xml.NewEncoder(w).Encode(&out)
}

// validFreeBusyRequest is METHOD:REQUEST with exactly one VFREEBUSY carrying ORGANIZER, at
// least one ATTENDEE, DTSTART and DTEND, beside any VTIMEZONE.
func validFreeBusyRequest(cal *ical.Calendar) bool {
	if p := cal.Props.Get(ical.PropMethod); p == nil || !strings.EqualFold(p.Value, "REQUEST") {
		return false
	}
	n := 0
	for _, c := range cal.Children {
		switch c.Name {
		case ical.CompTimezone:
		case ical.CompFreeBusy:
			n++
			for _, name := range []string{ical.PropOrganizer, ical.PropAttendee, ical.PropDateTimeStart, ical.PropDateTimeEnd} {
				if c.Props.Get(name) == nil {
					return false
				}
			}
		default:
			return false
		}
	}
	return n == 1
}
