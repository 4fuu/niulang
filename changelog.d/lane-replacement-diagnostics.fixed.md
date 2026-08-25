Flow completion records now report what the flow's lane replacements did:
`lane_replacement_waits`, `lane_replacement_timeouts`, `lane_replacement_wait`
and `lanes_joined`, written by both the client and the gateway under the same
names. A flow that fails with "lane replacement timeout" previously recorded
only that it had given up, which was 94% of the failures observed on a live
gateway over two hours and could not be diagnosed after the fact: the record
did not say whether a replacement lane had ever been offered, how many graces
the flow burned before failing, or how long it spent with no lane at all.
Distinguishing a pool that will not rebuild from a path that will not carry a
handshake needs all four, and none of them could be recovered from the logs as
they stood.
