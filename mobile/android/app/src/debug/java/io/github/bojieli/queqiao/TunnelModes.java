package io.github.bojieli.queqiao;

import java.util.List;

/**
 * The modes the debug app offers. Export mode comes first because it is what
 * the released app does; the full-device tunnel follows because this build is
 * the only vehicle that exercises the Go packet stack end to end on Android.
 *
 * This file exists once per build type. That duplication is the seam: it is the
 * single place the two variants disagree about what the app can do, and keeping
 * it a whole file rather than a build-config flag means the release build never
 * compiles the tunnel at all.
 */
final class TunnelModes {
    private TunnelModes() {
    }

    static List<TunnelController> available(TunnelHost host) {
        return List.of(new ProxyTunnelController(host), new VpnTunnelController(host));
    }
}
