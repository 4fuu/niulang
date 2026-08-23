`queqiao_account_admission_refused_total` by reason, and an `account flow
open refused` log record at `warn` naming the reason, account, and device.
Account admission was previously the one admission decision the gateway made
silently: no record at any log level and no counter, so a gateway refusing
half an account's connections was indistinguishable from a healthy one.
