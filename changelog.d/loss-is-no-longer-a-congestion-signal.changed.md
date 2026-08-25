Loss is no longer a congestion signal. The controller previously separated
erasure from congestion statistically and forwarded only the share the
channel could not explain, so that a path erasing packets independently of
the sending rate would not be read as an overloaded one. That separation is
gone: it was a statistical answer to a question the delivery-versus-rate
curve answers directly, and it was least reliable exactly when loss was
worst, because heavy erasure is bursty and the burst test then refused to
call it erasure at all. The brake is the delay bound instead.

Every loss now reaches the congestion controller, which matters for a reason
unrelated to congestion: its in-flight accounting is what was sent less what
was acknowledged and what was lost, so a controller told about only a
fraction of the losses believes the pipe is fuller than it is. The erasure
compensation rides on the measured erasure rather than on a floor biased low
for pacing, and waits for a measured round trip before engaging, because
that is when the delay bound can bound it.

queqiao_quic_controller_erasure_floor_ratio and
queqiao_quic_loss_suppressed_packets_total are removed. There is no floor
any more, and nothing is withheld from the controller, so neither figure can
be produced. queqiao_erasure_ratio{direction="send"} is the measurement that
replaced the first.
