package io.github.bojieli.queqiao;

import android.app.Service;
import android.content.Intent;
import android.os.IBinder;

import java.io.IOException;
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
 */
public final class QueqiaoProxyService extends Service implements TunnelServiceCore.Backend {
    static final String MODE = "proxy";

    private final TunnelServiceCore core = new TunnelServiceCore(this, this);
    private volatile String listenAddress;

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
        // No Protector: without a VpnService there is no interface to be exempt
        // from, and the core must not be told otherwise.
        Session session = Mobilecore.newSession(observer, null);
        session.startProxy(
                profile.profileJson,
                endpoint.listenAddress(),
                endpoint.username,
                endpoint.password);
        listenAddress = session.listenAddress();
        return session;
    }

    @Override
    public void release() {
        listenAddress = null;
    }

    @Override
    public String notificationDetail() {
        String address = listenAddress;
        return address == null ? "SOCKS5 endpoint" : "SOCKS5 " + address;
    }

    @Override
    public void onDestroy() {
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
