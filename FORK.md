# go-webdav fork

Upstream: github.com/emersion/go-webdav v0.7.0 (MIT, see LICENSE). Module path unchanged;
KyCalendar wires it with a `replace` in its go.mod.
Source of truth: github.com/Busnes-app/go-webdav (branch `kycalendar`).
KyCalendar vendors this branch byte for byte (without `.github`) in `third_party/go-webdav` and
pins the commit in `third_party/go-webdav.commit`; `scripts/check-fork-sync.sh` there fails on
any difference. Land a change here first, then sync it into KyCalendar.

## AI/LLM provenance

The CalDAV patches in this fork were written with large-language-model assistance and
autonomous coding agents; the git history discloses this (`Co-Authored-By` trailers).
They are **deliberately not submitted to emersion/go-webdav**: its contribution guidelines
ask for human-authored, non-autonomous, DCO-signed patches, and these do not meet that bar.
This repository exists so the code is available to anyone who wants it, and so KyCalendar has
a stable base to vendor. Provided as-is under the MIT license (see `LICENSE`).

**De-fork policy.** The fork tracks upstream and shrinks over time: when upstream ships an
equivalent of a patch below, that patch is dropped here and the fork rebased onto the newer
upstream. (Upstream has since added a read-only `current-user-privilege-set`, overlapping the
privilege half of patch 1 — the first de-fork candidate.)

Patches below are documented for transparency; they are **not** being submitted
upstream (see AI/LLM provenance) and each is de-forked when upstream ships an equivalent:

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
14. PUT and outbox POST bodies with an iCalendar parameter value over 1 KiB are refused
    (`valid-calendar-data`) before go-ical decodes them (`longParams`, caldav/params.go, which
    reads lines, folds and parameters as go-ical's decoder does). go-ical builds parameter values
    a byte at a time: one 512 KiB value inside a small body costs ~14 s of CPU.
