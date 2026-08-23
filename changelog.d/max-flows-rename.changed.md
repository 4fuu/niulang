`provider add-user --max-sessions` is renamed `--max-flows`, and now defaults
to 1024 instead of deferring to the gateway ceiling. It counts concurrent
flows -- one TCP connection or one UDP association each -- and never counted
devices. A browser opens roughly six connections per host across dozens of
hosts per page and holds them for the flow idle timeout, so a value chosen as
though it were a device count fails in the least legible way available: most
sites load and a few do not. The old flag name still works and warns.
Existing accounts keep the limit they were given; correct one with
`provider set-user-limits`.
