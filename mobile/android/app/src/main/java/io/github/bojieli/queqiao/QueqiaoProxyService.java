package io.github.bojieli.queqiao;

import android.app.Service;
import android.content.Intent;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.os.IBinder;

import java.io.IOException;
import java.util.Locale;
import java.util.function.BooleanSupplier;

import mobilecore.Mobilecore;
import mobilecore.Observer;
import mobilecore.Session;

/**
 * Exports the gateway as a local SOCKS5 endpoint and nothing else. Another VPN
 * client — v2rayNG, mihomo, sing-box — owns the device's routing rules and
 * treats this endpoint as one outbound among many.
 *
 * This service holds no VpnService, so it cannot call protect() on its own
 * sockets. The consumer app has to exclude Queqiao's package from its tunnel,
 * or Queqiao's uplink is captured by that tunnel and the connection loops.
 * VpnExclusion watches for exactly that, both at connect time and for as long
 * as the listener is open, because the usual order of events is that the
 * consumer's tunnel comes up second.
 */
public final class QueqiaoProxyService extends Service implements TunnelServiceCore.Backend {
    static final String MODE = "proxy";

    private final TunnelServiceCore core = new TunnelServiceCore(this, this);
    private volatile String listenAddress;
    private volatile VpnExclusion.State exclusion = VpnExclusion.State.UNKNOWN;
    private ConnectivityManager.NetworkCallback networkWatch;

    @Override
    public void onCreate() {
        super.onCreate();
        core.onCreate();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        return core.onStartCommand(intent, startId);
    }

    @Override
    public String modeId() {
        return MODE;
    }

    @Override
    public Session start(
            ProfileRepository.ActiveProfile profile,
            Observer observer,
            BooleanSupplier stillCurrent) throws Exception {
        ProxyEndpoint endpoint = ProxyEndpoint.load(this);
        if (!stillCurrent.getAsBoolean()) {
            throw new IOException("The connection was superseded before the listener was opened");
        }
        exclusion = VpnExclusion.current(this);
        if (exclusion == VpnExclusion.State.CAPTURED) {
            observer.onLog("WARN", VpnExclusion.WARNING);
        }
        // No Protector: without a VpnService there is no interface to be exempt
        // from, and the core must not be told otherwise.
        Session session = Mobilecore.newSession(observer, null);
        try {
            session.startProxy(
                    profile.profileJson,
                    endpoint.listenAddress(),
                    endpoint.username,
                    endpoint.password);
        } catch (Exception exception) {
            throw listenFailure(endpoint.port, exception);
        }
        listenAddress = session.listenAddress();
        watchForCapture();
        return session;
    }

    @Override
    public void release() {
        listenAddress = null;
        // Reset with the listener: a warning about a session that has ended
        // would otherwise outlive the session it was about.
        exclusion = VpnExclusion.State.UNKNOWN;
        stopWatchingForCapture();
    }

    @Override
    public String notificationDetail() {
        String address = listenAddress;
        String detail = address == null ? "SOCKS5 endpoint" : "SOCKS5 " + address;
        return exclusion == VpnExclusion.State.CAPTURED ? detail + " · VPN not excluded" : detail;
    }

    /**
     * Names the one listener failure a user can act on.
     *
     * The port is theirs to choose and loopback is shared with every other app,
     * so a taken port is an ordinary outcome. The Go error says "bind: address
     * already in use", which is true and useless — it names neither the port
     * nor the setting that changes it.
     */
    static Exception listenFailure(int port, Exception cause) {
        String message = cause.getMessage();
        String lowered = message == null ? "" : message.toLowerCase(Locale.ROOT);
        if (lowered.contains("address already in use") || lowered.contains("address in use")) {
            return new IOException(
                    "Port " + port + " is already used by another app on this device. "
                            + "Choose a different port in Settings, then connect again.",
                    cause);
        }
        return cause;
    }

    /**
     * Notices a VPN that comes up after the listener is already open.
     *
     * That ordering is the common one — Queqiao is connected first, then the
     * routing client — so a check made only at connect time would miss the case
     * it exists for.
     */
    private void watchForCapture() {
        ConnectivityManager manager = getSystemService(ConnectivityManager.class);
        if (manager == null || networkWatch != null) {
            return;
        }
        ConnectivityManager.NetworkCallback callback = new ConnectivityManager.NetworkCallback() {
            @Override
            public void onCapabilitiesChanged(Network network, NetworkCapabilities capabilities) {
                report(VpnExclusion.of(capabilities));
            }

            @Override
            public void onLost(Network network) {
                report(VpnExclusion.State.UNKNOWN);
            }
        };
        try {
            manager.registerDefaultNetworkCallback(callback);
            networkWatch = callback;
        } catch (RuntimeException exception) {
            // Watching is a courtesy. A connection that works must not be lost
            // because the platform declined to describe its network.
            networkWatch = null;
        }
    }

    private void stopWatchingForCapture() {
        ConnectivityManager.NetworkCallback callback = networkWatch;
        networkWatch = null;
        if (callback == null) {
            return;
        }
        ConnectivityManager manager = getSystemService(ConnectivityManager.class);
        if (manager == null) {
            return;
        }
        try {
            manager.unregisterNetworkCallback(callback);
        } catch (RuntimeException ignored) {
            // Already unregistered, or the service is being torn down.
        }
    }

    /** Reports only a change, so a stable network does not repeat itself. */
    private void report(VpnExclusion.State observed) {
        VpnExclusion.State previous = exclusion;
        exclusion = observed;
        if (observed == VpnExclusion.State.CAPTURED && previous != VpnExclusion.State.CAPTURED) {
            core.advise(VpnExclusion.WARNING);
        } else if (previous == VpnExclusion.State.CAPTURED && observed == VpnExclusion.State.EXCLUDED) {
            core.advise("Queqiao is no longer inside another app's VPN.");
        }
    }

    @Override
    public void onDestroy() {
        stopWatchingForCapture();
        core.onDestroy();
        super.onDestroy();
    }

    @Override
    public void onTrimMemory(int level) {
        super.onTrimMemory(level);
        core.onTrimMemory(level);
    }

    @Override
    public void onLowMemory() {
        super.onLowMemory();
        core.onLowMemory();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
