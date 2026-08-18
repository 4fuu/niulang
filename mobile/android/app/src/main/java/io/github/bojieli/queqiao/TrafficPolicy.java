package io.github.bojieli.queqiao;

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
