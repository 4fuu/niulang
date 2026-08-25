Flow completion records now say whether a replacement lane was ever attempted,
not only whether one arrived: `lane_replacement_attempts` and
`lane_replacement_failures` join the fields already written by both endpoints.
On a live gateway, 84% of the flows that failed with "lane replacement
timeout" ended with no replacement lane admitted, and the record could not say
why. A client pool that will not rebuild opens nothing; a path that will not
carry a handshake opens dials that never complete. Those need different fixes,
and both leave `lanes_joined` unchanged, because a dial that fails never
reaches lane admission. The client's own failure log now also carries the
`flow_id` and attempt number, so the attempts behind a failed flow can be
found rather than inferred.
