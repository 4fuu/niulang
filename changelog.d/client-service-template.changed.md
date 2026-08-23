`deploy/me.01.queqiao.client.plist` is removed. It was a macOS-only template
whose `/Users/YOU/.config/queqiao/PROVIDER-ID.json` placeholder pointed at a
path `queqiaod enroll` never writes on macOS, where the profile goes to
`~/Library/Application Support/queqiao/` instead; its header also recommended
`launchctl load` and `kickstart`, which the guide correctly warned against.
`queqiaod service print` renders the same file with real paths.
