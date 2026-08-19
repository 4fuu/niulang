package io.github.bojieli.queqiao;

import java.util.List;

/**
 * The modes the released app offers. There is one: Queqiao serves a local
 * SOCKS5 endpoint and a routing app decides what to send into it.
 *
 * The full-device tunnel lives in the debug source set only. Shipping it would
 * put Queqiao in competition with mature routing clients over rules, DNS, and
 * per-app policy — work the project's own doctrine assigns to a larger overlay,
 * not to the data plane. It would also make the released build declare
 * BIND_VPN_SERVICE, which carries obligations this app has no reason to take
 * on.
 */
final class TunnelModes {
    private TunnelModes() {
    }

    static List<TunnelController> available(TunnelHost host) {
        return List.of(new ProxyTunnelController(host));
    }
}
