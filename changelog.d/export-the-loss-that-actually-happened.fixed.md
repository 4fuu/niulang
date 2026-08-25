The metrics now report the loss the path actually caused.
`queqiao_quic_loss_observed_packets_total` counts every loss the sender
detected. Only the share charged to the congestion controller used to leave
the process, and on an erasure path that is a small fraction of what
happened: against a channel erasing a fifth of its packets the exported
figure read under two percent, because the rest had been reclassified as
erasure and then dropped from the record. Divide the observed count by
`queqiao_quic_packets_sent` for a loss rate. That reclassification is itself
gone in this release, so the two figures now agree; the observed counter is
what stays true if a controller ever again declines to count something.

`queqiao_quic_packets_lost` and `queqiao_quic_bytes_lost` are removed, along
with the visualizer's byte-loss series that was derived from them. They were
quic-go's own counters, which it increments only inside its cubic sender,
and this transport replaces the congestion controller -- so nothing could
ever move them off zero while the dashboard divided by them for its loss
chart. A counter that cannot be produced is worse than a missing one once it
is monotonic, because it then reads as a measurement rather than as an
absence.
