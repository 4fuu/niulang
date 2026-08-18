package io.github.bojieli.queqiao;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.net.VpnService;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.os.ParcelFileDescriptor;

import java.io.IOException;
import java.security.GeneralSecurityException;
import java.util.concurrent.atomic.AtomicBoolean;

import mobilecore.Mobilecore;
import mobilecore.Observer;
import mobilecore.Protector;
import mobilecore.Session;

public final class QueqiaoVpnService extends VpnService implements Observer, Protector {
    static final String ACTION_CONNECT = "io.github.bojieli.queqiao.CONNECT";
    static final String ACTION_DISCONNECT = "io.github.bojieli.queqiao.DISCONNECT";
    static final String ACTION_STATUS = "io.github.bojieli.queqiao.STATUS";
    static final String ACTION_STATE = "io.github.bojieli.queqiao.STATE";
    static final String EXTRA_STATE = "state";
    static final String EXTRA_MESSAGE = "message";
    static final String EXTRA_METRICS = "metrics";
    static final String EXTRA_PROFILE_ID = "profile_id";

    private static final String NOTIFICATION_CHANNEL = "queqiao_vpn";
    private static final int NOTIFICATION_ID = 1001;
    private static final int MTU = 1280;
    private static final long METRICS_INTERVAL_MILLIS = 1000;

    private final Object lifecycleLock = new Object();
    private final AtomicBoolean stopping = new AtomicBoolean();
    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private boolean startInProgress;
    private Session session;
    private ParcelFileDescriptor tunnel;
    private String activeProfileId;
    private String activeProfileName;

    private final Runnable metricsPublisher = new Runnable() {
        @Override
        public void run() {
            String state;
            String metrics;
            synchronized (lifecycleLock) {
                if (session == null) {
                    return;
                }
                state = session.state();
                metrics = session.metricsJSON();
            }
            publishState(state, null, metrics);
            if (Mobilecore.StateRunning.equals(state)) {
                mainHandler.postDelayed(this, METRICS_INTERVAL_MILLIS);
            }
        }
    };

    @Override
    public void onCreate() {
        super.onCreate();
        createNotificationChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? null : intent.getAction();
        if (ACTION_DISCONNECT.equals(action)) {
            stopTunnel(null);
            return START_NOT_STICKY;
        }
        if (ACTION_STATUS.equals(action)) {
            publishCurrentStatus();
            synchronized (lifecycleLock) {
                if (session == null && !startInProgress) {
                    stopSelf(startId);
                }
            }
            return START_NOT_STICKY;
        }
        if (!ACTION_CONNECT.equals(action)) {
            stopSelf(startId);
            return START_NOT_STICKY;
        }

        String requestedProfileId = intent.getStringExtra(EXTRA_PROFILE_ID);
        if (requestedProfileId == null || requestedProfileId.isBlank()) {
            publishState(Mobilecore.StateFailed, "No Queqiao profile was selected", null);
            stopSelf(startId);
            return START_NOT_STICKY;
        }

        startForeground(NOTIFICATION_ID, notification("Starting", false));
        synchronized (lifecycleLock) {
            if (session != null) {
                if (!requestedProfileId.equals(activeProfileId)) {
                    publishState(
                            session.state(),
                            "Disconnect before switching the active Queqiao profile",
                            session.metricsJSON());
                } else {
                    publishState(session.state(), null, session.metricsJSON());
                }
                return START_STICKY;
            }
            if (startInProgress || stopping.get()) {
                publishState(Mobilecore.StateStarting, null, null);
                return START_STICKY;
            }
            startInProgress = true;
            activeProfileId = requestedProfileId;
        }
        Thread worker = new Thread(
                () -> startTunnel(requestedProfileId),
                "queqiao-vpn-start");
        worker.start();
        return START_STICKY;
    }

    private void startTunnel(String profileId) {
        try {
            ProfileRepository repository = new ProfileRepository(this);
            ProfileRepository.ActiveProfile active = repository.profile(profileId);
            if (active == null) {
                throw new GeneralSecurityException("The selected Queqiao profile no longer exists");
            }
            String profile = active.profileJson;
            if (Mobilecore.profileNeedsRenewal(profile) != 0) {
                profile = Mobilecore.renewProfile(profile);
                Mobilecore.validateProfile(profile);
                repository.replaceProfile(profileId, profile);
                active = repository.profile(profileId);
                if (active == null) {
                    throw new GeneralSecurityException("The renewed Queqiao profile is unavailable");
                }
            }
            activeProfileName = active.record.displayName;
            Builder builder = new Builder()
                    .setSession("Queqiao — " + active.record.displayName)
                    .setMtu(MTU)
                    // Excluding our UID ensures identity renewal cannot re-enter the VPN.
                    // Individual outer sockets are protected as an independent boundary.
                    .addDisallowedApplication(getPackageName())
                    .addAddress("10.77.0.2", 32)
                    .addAddress("fd77:7171:6f::2", 128)
                    .addDnsServer("1.1.1.1")
                    .addDnsServer("2606:4700:4700::1111")
                    .setBlocking(false);
            RoutePolicy.apply(builder, active.record.trafficPolicy);
            ParcelFileDescriptor established = builder.establish();
            if (established == null) {
                throw new IOException("Android refused to establish the VPN interface");
            }
            Session newSession = Mobilecore.newSession(this, this);
            synchronized (lifecycleLock) {
                if (stopping.get()) {
                    established.close();
                    return;
                }
                tunnel = established;
                session = newSession;
            }
            newSession.start(profile, established.getFd(), 0, MTU, true);
        } catch (Exception exception) {
            publishState(Mobilecore.StateFailed, safeMessage(exception), null);
            stopTunnel("Connection failed");
        } finally {
            synchronized (lifecycleLock) {
                startInProgress = false;
            }
        }
    }

    @Override
    public boolean protect(long fileDescriptor) {
        return fileDescriptor >= 0
                && fileDescriptor <= Integer.MAX_VALUE
                && protect((int) fileDescriptor);
    }

    @Override
    public void onStateChanged(String state) {
        publishState(state, null, currentMetrics());
        NotificationManager manager = getSystemService(NotificationManager.class);
        manager.notify(
                NOTIFICATION_ID,
                notification(displayState(state), Mobilecore.StateRunning.equals(state)));
        if (Mobilecore.StateRunning.equals(state)) {
            mainHandler.removeCallbacks(metricsPublisher);
            mainHandler.post(metricsPublisher);
        } else if (Mobilecore.StateFailed.equals(state)) {
            stopTunnel("Connection failed");
        }
    }

    @Override
    public void onLog(String level, String message) {
        if ("ERROR".equalsIgnoreCase(level)) {
            publishState(currentState(), message, currentMetrics());
        }
    }

    @Override
    public boolean onProfileUpdated(String profileJson) {
        String profileId;
        synchronized (lifecycleLock) {
            profileId = activeProfileId;
        }
        if (profileId == null) {
            return false;
        }
        try {
            new ProfileRepository(this).replaceProfile(profileId, profileJson);
            return true;
        } catch (Exception exception) {
            publishState(
                    currentState(),
                    "Could not persist renewed device identity; renewal will be retried",
                    currentMetrics());
            return false;
        }
    }

    @Override
    public void onRevoke() {
        stopTunnel("VPN permission revoked");
        super.onRevoke();
    }

    @Override
    public void onDestroy() {
        boolean hasTunnel;
        synchronized (lifecycleLock) {
            hasTunnel = session != null || tunnel != null || startInProgress;
        }
        if (hasTunnel) {
            stopTunnel(null);
        } else {
            mainHandler.removeCallbacks(metricsPublisher);
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return super.onBind(intent);
    }

    private void stopTunnel(String reason) {
        if (!stopping.compareAndSet(false, true)) {
            return;
        }
        mainHandler.removeCallbacks(metricsPublisher);
        Session oldSession;
        ParcelFileDescriptor oldTunnel;
        synchronized (lifecycleLock) {
            oldSession = session;
            oldTunnel = tunnel;
            session = null;
            tunnel = null;
        }
        if (oldSession != null) {
            try {
                oldSession.stop();
            } catch (Exception ignored) {
                // The state callback has already surfaced the failure.
            }
        }
        if (oldTunnel != null) {
            try {
                oldTunnel.close();
            } catch (IOException ignored) {
                // Descriptor teardown is best effort after the core has stopped.
            }
        }
        publishState(Mobilecore.StateStopped, reason, null);
        synchronized (lifecycleLock) {
            activeProfileId = null;
            activeProfileName = null;
        }
        stopForeground(STOP_FOREGROUND_REMOVE);
        stopSelf();
    }

    private void publishCurrentStatus() {
        publishState(currentState(), null, currentMetrics());
    }

    private void publishState(String state, String message, String metrics) {
        Intent update = new Intent(ACTION_STATE)
                .setPackage(getPackageName())
                .putExtra(EXTRA_STATE, state);
        synchronized (lifecycleLock) {
            if (activeProfileId != null) {
                update.putExtra(EXTRA_PROFILE_ID, activeProfileId);
            }
        }
        if (message != null) {
            update.putExtra(EXTRA_MESSAGE, message);
        }
        if (metrics != null) {
            update.putExtra(EXTRA_METRICS, metrics);
        }
        sendBroadcast(update);
    }

    private String currentState() {
        synchronized (lifecycleLock) {
            if (session != null) {
                return session.state();
            }
            return startInProgress ? Mobilecore.StateStarting : Mobilecore.StateStopped;
        }
    }

    private String currentMetrics() {
        synchronized (lifecycleLock) {
            return session == null ? null : session.metricsJSON();
        }
    }

    private Notification notification(String state, boolean connected) {
        Intent open = new Intent(this, MainActivity.class);
        PendingIntent contentIntent = PendingIntent.getActivity(
                this,
                0,
                open,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        Intent disconnect = new Intent(this, QueqiaoVpnService.class)
                .setAction(ACTION_DISCONNECT);
        PendingIntent disconnectIntent = PendingIntent.getService(
                this,
                1,
                disconnect,
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        String detail = activeProfileName == null ? state : activeProfileName + " · " + state;
        Notification.Builder builder = new Notification.Builder(this, NOTIFICATION_CHANNEL)
                .setSmallIcon(R.drawable.ic_queqiao)
                .setContentTitle("Queqiao")
                .setContentText(detail)
                .setContentIntent(contentIntent)
                .setCategory(Notification.CATEGORY_SERVICE)
                .setOngoing(connected)
                .setOnlyAlertOnce(true);
        if (connected) {
            builder.addAction(
                    new Notification.Action.Builder(null, "Disconnect", disconnectIntent).build());
        }
        return builder.build();
    }

    private void createNotificationChannel() {
        NotificationChannel channel = new NotificationChannel(
                NOTIFICATION_CHANNEL,
                getString(R.string.vpn_channel_name),
                NotificationManager.IMPORTANCE_LOW);
        channel.setDescription(getString(R.string.vpn_channel_description));
        getSystemService(NotificationManager.class).createNotificationChannel(channel);
    }

    private static String displayState(String state) {
        if (state == null || state.isEmpty()) {
            return "Unknown";
        }
        return Character.toUpperCase(state.charAt(0)) + state.substring(1);
    }

    private static String safeMessage(Exception exception) {
        String message = exception.getMessage();
        return message == null || message.isBlank()
                ? exception.getClass().getSimpleName()
                : message;
    }
}
