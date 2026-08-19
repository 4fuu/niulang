package io.github.bojieli.queqiao;

import android.app.Activity;

/**
 * What a TunnelController may ask of the screen that hosts it: the
 * activity to build views against, the profile store, a worker thread, and the
 * two ways a controller reports back — a failure dialog or a redraw.
 */
interface TunnelHost {
    Activity activity();

    ProfileRepository repository();

    /** Runs work off the main thread on the activity's single worker. */
    void background(Runnable work);

    /** Surfaces a failure to the user and clears any pending busy state. */
    void failure(String title, Exception exception);

    /** Re-reads the profile catalog and redraws the current page. */
    void refresh();

    /** True while a connection is running or in transition. */
    boolean connectionActive();
}
