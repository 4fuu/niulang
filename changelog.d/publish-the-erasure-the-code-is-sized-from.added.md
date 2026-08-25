The erasure a path is measured to be applying is now published, labelled by
the direction it was measured on: queqiao_erasure_ratio{direction="send"} in
the metrics and queqiao_erasure_ratio_send in the performance snapshot. A
gateway's send direction is its downstream, which is the direction that had
no metric at all. The only erasure figure that used to leave the process was
the congestion controller's floor, and a floor is not a smaller version of
the measurement: it is biased low so that pacing errs towards slowing down,
and it is a lower envelope for a connection's lifetime, so it keeps whatever
a clean window established while the channel moves. On one live incident the
two read 1.76% and 19.9%, and nothing on a dashboard could show which one
the code was being sized for.
