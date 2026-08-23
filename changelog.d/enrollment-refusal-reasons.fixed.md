Enrollment refusals say which of the two possible things went wrong, and say
it where an operator can see it. Every failure used to collapse into one
sentence sent only to the client - `invitation is invalid, expired, already
used, or unavailable` - covering both a genuinely spent invitation and a
gateway that could not open its own authorization store. Nothing was logged
server-side for a refused or an accepted enrollment at any level, and the
gateway discarded the result at every call site, so a gateway refusing every
enrollment it received looked exactly like one receiving none. A store that
cannot be read, locked, or written is now reported as a temporary
unavailability, recorded at error level, and never described to the client as
a verdict on their invitation; a real refusal is recorded at warning level;
an acceptance is recorded with the account and device it created. Records are
rate-limited per outcome and carry the suppressed and total counts, so a
storm stays one readable line. Enrollment attempts dropped because the
enrollment slots were full are also reported now, matching the session and
connection ceilings beside them.
