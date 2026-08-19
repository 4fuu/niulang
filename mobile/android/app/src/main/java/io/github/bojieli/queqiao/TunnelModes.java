package io.github.bojieli.queqiao;

import java.util.List;

/**
 * The modes this build offers, most preferred first. The list is the one place
 * the two build variants differ in the activity's view of the world.
 */
final class TunnelModes {
    private TunnelModes() {
    }

    static List<TunnelController> available(TunnelHost host) {
        return List.of(new VpnTunnelController(host));
    }
}
