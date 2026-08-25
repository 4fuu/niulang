Recover UDP associations after a pooled QUIC stream stops making progress, and
replace connections bound to a DHCP address which disappeared after a network
change, instead of leaving either path stalled until the client restarts.
