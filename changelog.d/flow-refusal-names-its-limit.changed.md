A flow open refused by an account limit now says which limit refused it:
`account flow limit reached` or `account device limit reached`, replacing
`account session unavailable`. A device that lost authorization between its
handshake and its next flow open is answered with the AUTHENTICATION reset
code and `device is not authorized` rather than being reported as a limit.
