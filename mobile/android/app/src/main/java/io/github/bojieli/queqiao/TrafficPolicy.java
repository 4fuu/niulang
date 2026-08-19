package io.github.bojieli.queqiao;

/**
 * How the full-device tunnel divides traffic.
 *
 * Only the debug build acts on this — the released app routes nothing and hands
 * the decision to the consumer's routing client. It stays in the main source
 * set because the profile catalog is one JSON document written by both build
 * types, and a value the release build could not parse would make a catalog
 * saved by the debug build unreadable on the same device.
 */
enum TrafficPolicy {
    ALL_TRAFFIC(
            "all-traffic",
            "All traffic",
            "Route IPv4, IPv6, and DNS traffic through the selected Queqiao provider."),
    EXCLUDE_LOCAL_NETWORKS(
            "exclude-local-networks",
            "Exclude local networks",
            "Keep private and link-local destinations outside the tunnel; route internet and DNS traffic through Queqiao.");

    final String wireValue;
    final String title;
    final String detail;

    TrafficPolicy(String wireValue, String title, String detail) {
        this.wireValue = wireValue;
        this.title = title;
        this.detail = detail;
    }

    static TrafficPolicy fromWireValue(String value) {
        for (TrafficPolicy policy : values()) {
            if (policy.wireValue.equals(value)) {
                return policy;
            }
        }
        return ALL_TRAFFIC;
    }
}
