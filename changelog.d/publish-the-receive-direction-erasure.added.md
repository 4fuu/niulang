The erasure an endpoint receives is now published as well as the erasure it
sends into. queqiao_coded_symbols_total is split by outcome -- arrived,
recovered by the code, or left the window still missing -- with
queqiao_erasure_ratio{direction="receive"} and
queqiao_erasure_residual_ratio{direction="receive"} derived from those
counters. The residual is what the code could not repair and the session re-
issues a round trip later, and it is the figure the motivating incident was
actually made of: 11% of the downstream payload, measured by the client's
decoders on every flow and never leaving the process. Because every source
symbol the peer sent ends in exactly one outcome, the three counters are a
denominator, so the ratios are counters over counters rather than a mean of
per-flow ratios that would weight a kilobyte flow the same as a gigabyte
one.
