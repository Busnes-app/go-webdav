# go-webdav fork

Upstream: github.com/emersion/go-webdav v0.7.0 (MIT, see LICENSE). Module path unchanged;
KyCalendar wires it with a `replace` in its go.mod.
Source of truth for review and upstreaming: github.com/Busnes-app/go-webdav (branch `kycalendar`).
KyCalendar vendors this branch byte for byte (without `.github`) in `third_party/go-webdav` and
pins the commit in `third_party/go-webdav.commit`; `scripts/check-fork-sync.sh` there fails on
any difference. Land a change here first, then sync it into KyCalendar.

Patches (each one is a candidate upstream PR):

1. Calendar colour, getctag, sync-token, supported-report-set and per-calendar read-only privileges.
2. Raw iCalendar bytes on PUT and GET; max-resource-size precondition.
3. RFC 6578 sync-collection REPORT through an optional SyncBackend.
4. PROPPATCH through an optional CalendarUpdater; MKCALENDAR.
5. Bounded recurrence expansion in time-range matching (cap 100000 occurrences; a rule whose
   BYHOUR x BYMINUTE x BYSECOND exceeds 96 matches conservatively without expansion).
6. calendar-multiget answers each distinct href once: every response holds a full object copy
   until the multistatus is written.
7. Time-range matching guards rules rrule-go cannot bound (`stalls`, match.go): it checks UNTIL
   only on a matching candidate, so BYSETPOS on HOURLY, MINUTELY or SECONDLY steps every period
   to year 9999, and a step that never lands on its BYHOUR/BYMINUTE/BYSECOND clock loops forever.
   Both match conservatively without expansion. Direct dependency on rrule-go v1.8.2.
8. Non-recurring time-range matching follows RFC 4791 §9.9 exactly: `start < DTEND AND end >
   DTSTART`, and a zero-length event matches at the range start.
9. PROPFIND with `Depth: infinity`, or with no Depth (infinity per RFC 4918 §9.1), is 403 with
   the `DAV:propfind-finite-depth` precondition.
10. MKCALENDAR passes `supported-calendar-component-set` to the backend in
    `Calendar.SupportedComponentSet`; the backend decides which sets it accepts.
11. VTODO time-range matching follows RFC 4791 §9.9's VTODO table (DTSTART with DURATION or
    DUE, DUE alone, COMPLETED and CREATED, else always).
12. CALDAV:free-busy-query REPORT (RFC 4791 §7.10) through an optional FreeBusyBackend, which
    returns busy periods only; the handler clips them to the range, merges them per FBTYPE and
    renders one VFREEBUSY, so no event data reaches the reply. Calendar collections only;
    advertised in supported-report-set only when the backend implements it.
13. CalDAV scheduling surface (RFC 6638) through an optional SchedulingBackend whose
    Scheduling(ctx) returns nil when off; off, nothing below is advertised or served and a POST
    is answered as before. On: OPTIONS adds `calendar-auto-schedule`; the principal serves
    `calendar-user-address-set`, `schedule-inbox-URL` and `schedule-outbox-URL`; calendars serve
    `schedule-calendar-transp`; the inbox and outbox answer PROPFIND (resource type, privileges,
    `schedule-default-calendar-URL`); an outbox POST of a METHOD:REQUEST VFREEBUSY answers a
    `schedule-response` from ScheduleFreeBusy (409 `valid-scheduling-message` otherwise);
    `CalendarObject.ScheduleTag` is served as the Schedule-Tag header and `schedule-tag`
    property, and PUT passes If-Schedule-Tag-Match to the backend. `FreeBusyCalendar` (patch 12's
    clip-and-merge renderer) is exported for backends building outbox replies.
