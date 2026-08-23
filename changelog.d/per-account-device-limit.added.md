A per-account concurrent-device limit, `provider add-user --max-clients`,
defaulting to 8. A device counts once however many flows it carries, so this
is the limit that expresses "this account is for eight devices" -- which is
what the per-account limit was widely assumed to already mean.
