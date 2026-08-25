The bandwidth estimate can now come down. Its max filter kept a sample for
ten packet-timed rounds, which is the right clock only while rounds advance
-- and a round advances when a packet sent in the previous round is
acknowledged, so a connection with nothing to send advances none. Measured
on a live gateway, 99.98% of samples were application limited and the round
counter moved about nine times an hour, turning a ten-round memory into
sixty-six minutes of wall time. One burst through a shaper therefore set the
estimate for the rest of the hour: the gateway held 519 Mbit/s against a
measured sustained 17.6 and paced and sized its congestion window from that.

A sample now expires on rounds or on wall time, whichever comes first, with
the wall-clock window derived from the measured minimum round trip rather
than configured -- ten rounds and the time ten rounds takes are the same
statement on a path whose rounds are advancing. An application-limited
sample may also replace a standing estimate once that estimate has expired,
which it previously could not; without that a connection which is
application limited essentially always would have expired its estimate with
nothing able to take its place.
