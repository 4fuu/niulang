Provider state keeps its owner when a maintenance command runs under `sudo`.
State files are installed by renaming a replacement over the target, and the
replacement belonged to whoever ran the command, so one privileged
`queqiaod provider add-user` transferred `authorization.json` to `root` and
left the gateway's own service account unable to open it. The gateway then
logged `authorization refresh failed; retaining last known-good state` once a
second, kept admitting existing devices from its cached snapshot, and refused
every new enrollment - which surfaced to users as an invalid invitation
rather than as the server-side outage it was. Replacements now adopt the
owner of the file they replace, or of the state directory when the file is
new. The authorization lock is covered too: it is taken before the store is
read and is never removed, so a lock left behind by a privileged run refused
every later write.
