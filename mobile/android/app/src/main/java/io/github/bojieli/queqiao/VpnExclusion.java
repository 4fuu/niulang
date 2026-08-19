package io.github.bojieli.queqiao;

import android.content.Context;
import android.net.ConnectivityManager;
import android.net.NetworkCapabilities;

/**
 * Whether another app's VPN is carrying Queqiao's own uplink.
 *
 * Export mode rests on the consumer routing client excluding Queqiao from its
 * tunnel. Skip that step and the consumer's TUN captures Queqiao's uplink,
 * feeds it into the consumer's outbound, and that outbound is this app's
 * listener. Traffic then loops until it times out rather than failing
 * outright, which is the worst shape a failure can take: no error, no
 * throughput, and nothing on screen that names the cause.
 *
 * Android answers the question directly, because the default network it
 * reports is per-UID: a VPN that excluded this app is not this app's default
 * network. TRANSPORT_VPN here therefore means Queqiao was not excluded.
 *
 * The answer is advisory and never blocks a connection. A VPN carrying
 * Queqiao's traffic is not proof of a loop — a corporate VPN the gateway is
 * reachable through is a legitimate setup, and so is a consumer client whose
 * rules send the gateway address direct. It is the condition under which a
 * failure has one obvious cause, which is worth naming rather than leaving the
 * user to guess.
 */
final class VpnExclusion {

    enum State {
        /** No VPN applies to this app: either none is running, or it excluded Queqiao. */
        EXCLUDED,
        /** A VPN is this app's default network, so Queqiao's own uplink enters it. */
        CAPTURED,
        /** Android would not say — offline, or no ConnectivityManager. Never reported as a fault. */
        UNKNOWN
    }

    /** Said while connecting, or when a VPN appears underneath a live listener. */
    static final String WARNING =
            "Another VPN is carrying Queqiao's own traffic. Exclude Queqiao in that app's "
                    + "per-app proxy settings, or connections through this endpoint will loop.";

    /** Said when the connection test fails and this is the likely reason. */
    static final String DIAGNOSIS =
            "The provider could not be reached, and another VPN is carrying Queqiao's own "
                    + "traffic. Exclude Queqiao in that app's per-app proxy settings and test again.";

    private VpnExclusion() {
    }

    static State current(Context context) {
        ConnectivityManager manager = context.getSystemService(ConnectivityManager.class);
        if (manager == null) {
            return State.UNKNOWN;
        }
        try {
            return of(manager.getNetworkCapabilities(manager.getActiveNetwork()));
        } catch (RuntimeException exception) {
            // A connectivity query is a diagnostic aid, never a precondition:
            // failing to answer must not fail the connection it describes.
            return State.UNKNOWN;
        }
    }

    static State of(NetworkCapabilities capabilities) {
        if (capabilities == null) {
            return State.UNKNOWN;
        }
        return capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN)
                ? State.CAPTURED
                : State.EXCLUDED;
    }
}
