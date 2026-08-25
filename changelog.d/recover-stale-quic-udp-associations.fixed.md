Recover UDP associations after a pooled QUIC stream stops making progress, and
replace connections bound to a DHCP address which disappeared after a network
change. An observed interface outage also starts a new path when the address
after reconnect is unchanged, instead of leaving any of these paths stalled
until the client restarts.
