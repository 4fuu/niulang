The metrics now report the loss the path actually caused. Three counters are
published together: `queqiao_quic_loss_observed_packets_total` is every loss
the sender detected, `queqiao_quic_loss_suppressed_packets_total` is the
part withheld from the congestion controller as erasure, and the existing
`queqiao_quic_controller_packets_lost` is the part charged as congestion,
with observed being the sum of the other two. Only the charged figure used
to leave the process, and on an erasure path that is a small fraction of
what happened: against a channel erasing a fifth of its packets the exported
figure reads under two percent, because the rest had been correctly
reclassified as erasure and then dropped from the record. Divide observed by
`queqiao_quic_packets_sent` for a loss rate.

`queqiao_quic_packets_lost` and `queqiao_quic_bytes_lost` are removed, along
with the visualizer's byte-loss series that was derived from them. They were
quic-go's own counters, which it increments only inside its cubic sender,
and this transport replaces the congestion controller -- so nothing could
ever move them off zero while the dashboard divided by them for its loss
chart. A counter that cannot be produced is worse than a missing one once it
is monotonic, because it then reads as a measurement rather than as an
absence.
