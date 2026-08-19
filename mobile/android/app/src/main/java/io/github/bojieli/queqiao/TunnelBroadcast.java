package io.github.bojieli.queqiao;

/**
 * The state contract between a connection service and the activity. Both build
 * variants speak it, so the activity registers one receiver and still learns
 * from EXTRA_MODE which service produced the update.
 */
final class TunnelBroadcast {
    static final String ACTION_CONNECT = "io.github.bojieli.queqiao.CONNECT";
    static final String ACTION_DISCONNECT = "io.github.bojieli.queqiao.DISCONNECT";
    static final String ACTION_STATUS = "io.github.bojieli.queqiao.STATUS";
    static final String ACTION_STATE = "io.github.bojieli.queqiao.STATE";

    static final String EXTRA_STATE = "state";
    static final String EXTRA_MESSAGE = "message";
    static final String EXTRA_METRICS = "metrics";
    static final String EXTRA_PROFILE_ID = "profile_id";
    /** The reporting controller's mode identifier; see TunnelController.modeId. */
    static final String EXTRA_MODE = "mode";

    private TunnelBroadcast() {
    }
}
