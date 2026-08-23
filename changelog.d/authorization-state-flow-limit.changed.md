Provider authorization state writes the per-account flow limit as
`max_flows`. An existing state naming it `max_sessions` is read unchanged and
rewritten on the next save; a state naming both is refused rather than having
one silently win.
