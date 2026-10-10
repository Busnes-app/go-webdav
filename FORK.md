# go-webdav fork

Upstream: github.com/emersion/go-webdav v0.7.0 (MIT, see LICENSE). Module path unchanged;
KyCalendar wires it with a `replace` in its go.mod.
Source of truth for review and upstreaming: github.com/Busnes-app/go-webdav (branch `kycalendar`).

Patches (each one is a candidate upstream PR):

1. Calendar colour, getctag, sync-token, supported-report-set and per-calendar read-only privileges.
2. Raw iCalendar bytes on PUT and GET; max-resource-size precondition.
3. RFC 6578 sync-collection REPORT through an optional SyncBackend.
4. PROPPATCH through an optional CalendarUpdater; MKCALENDAR.
5. Bounded recurrence expansion in time-range matching (cap 100000 occurrences; a rule whose
   BYHOUR x BYMINUTE x BYSECOND exceeds 96 matches conservatively without expansion).
6. calendar-multiget answers each distinct href once: every response holds a full object copy
   until the multistatus is written.
