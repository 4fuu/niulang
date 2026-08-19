package io.github.bojieli.queqiao;

import org.json.JSONException;
import org.json.JSONObject;

import java.util.Locale;

final class ConnectionProbe {
    enum Status {
        TESTING,
        AVAILABLE,
        UNAVAILABLE
    }

    final Status status;
    final String transport;
    final long latencyMilliseconds;
    final String detail;

    private ConnectionProbe(
            Status status,
            String transport,
            long latencyMilliseconds,
            String detail) {
        this.status = status;
        this.transport = transport;
        this.latencyMilliseconds = latencyMilliseconds;
        this.detail = detail;
    }

    static ConnectionProbe testing() {
        return new ConnectionProbe(Status.TESTING, null, 0, null);
    }

    static ConnectionProbe available(String encoded) throws JSONException {
        JSONObject object = new JSONObject(encoded);
        if (object.getInt("version") != 1) {
            throw new JSONException("Unsupported connection-test result version");
        }
        String transport = object.getString("transport");
        if (!"quic".equals(transport) && !"tcp".equals(transport)) {
            throw new JSONException("Unknown connection-test transport");
        }
        long latency = object.getLong("latency_ms");
        if (latency <= 0) {
            throw new JSONException("Invalid connection-test latency");
        }
        return new ConnectionProbe(Status.AVAILABLE, transport, latency, null);
    }

    /**
     * A failed test, named as precisely as the device allows.
     *
     * The probe opens no remote destination — it measures DNS, transport setup,
     * mutual TLS, device authorization, and one control round trip — so a
     * failure here is about reaching the provider at all. When another app's
     * VPN is carrying Queqiao's own traffic, that is far and away the likeliest
     * reason, and saying so beats handing back a timeout the user has to
     * interpret. The underlying message is kept beneath it, because the
     * diagnosis is a strong guess rather than a proof.
     */
    static ConnectionProbe unavailable(Exception exception, VpnExclusion.State exclusion) {
        String message = exception.getMessage();
        if (message == null || message.isBlank()) {
            message = exception.getClass().getSimpleName();
        }
        if (exclusion == VpnExclusion.State.CAPTURED) {
            message = VpnExclusion.DIAGNOSIS + "\n\n" + message;
        }
        return new ConnectionProbe(Status.UNAVAILABLE, null, 0, message);
    }

    String summary() {
        switch (status) {
            case TESTING:
                return "Testing…";
            case AVAILABLE:
                return latencyMilliseconds + " ms · " + transport.toUpperCase(Locale.ROOT);
            case UNAVAILABLE:
                return "Unavailable";
            default:
                throw new IllegalStateException("Unknown connection-test status");
        }
    }
}
