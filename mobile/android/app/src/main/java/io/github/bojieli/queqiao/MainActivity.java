package io.github.bojieli.queqiao;

import android.Manifest;
import android.annotation.SuppressLint;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.BroadcastReceiver;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.net.Uri;
import android.net.VpnService;
import android.os.Build;
import android.os.Bundle;
import android.provider.Settings;
import android.text.InputType;
import android.view.Gravity;
import android.view.View;
import android.view.WindowInsets;
import android.widget.Button;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.RadioButton;
import android.widget.RadioGroup;
import android.widget.ScrollView;
import android.widget.TextView;

import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.text.DateFormat;
import java.time.Instant;
import java.util.Date;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

import mobilecore.Mobilecore;

public final class MainActivity extends Activity {
    private static final int REQUEST_VPN = 7001;
    private static final int REQUEST_NOTIFICATIONS = 7002;

    private enum Page {
        HOME,
        PROFILES,
        SETTINGS
    }

    private final ExecutorService worker = Executors.newSingleThreadExecutor(runnable -> {
        Thread thread = new Thread(runnable, "queqiao-app-work");
        thread.setDaemon(true);
        return thread;
    });
    private ProfileRepository repository;
    private ProfileRepository.Catalog catalog = new ProfileRepository.Catalog();
    private FrameLayout pageContainer;
    private Button homeTab;
    private Button profilesTab;
    private Button settingsTab;
    private TextView statusView;
    private TextView connectionSubtitle;
    private TextView downloadedView;
    private TextView uploadedView;
    private TextView flowsView;
    private Button connectionButton;
    private Page currentPage = Page.HOME;
    private String tunnelState = Mobilecore.StateStopped;
    private String tunnelMessage;
    private String serviceProfileId;
    private String pendingConnectProfileId;
    private boolean busy;
    private long bytesUp;
    private long bytesDown;
    private long activeFlows;

    private final BroadcastReceiver stateReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            if (!QueqiaoVpnService.ACTION_STATE.equals(intent.getAction())) {
                return;
            }
            tunnelState = valueOr(intent.getStringExtra(QueqiaoVpnService.EXTRA_STATE), Mobilecore.StateStopped);
            tunnelMessage = intent.getStringExtra(QueqiaoVpnService.EXTRA_MESSAGE);
            serviceProfileId = intent.getStringExtra(QueqiaoVpnService.EXTRA_PROFILE_ID);
            parseMetrics(intent.getStringExtra(QueqiaoVpnService.EXTRA_METRICS));
            busy = isTransitioning();
            renderConnectionState();
        }
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        repository = new ProfileRepository(this);
        setContentView(buildShell());
        showPage(Page.HOME);
        refreshCatalog();
        handleIncomingIntent(getIntent());
    }

    @Override
    @SuppressLint("InlinedApi")
    protected void onStart() {
        super.onStart();
        IntentFilter filter = new IntentFilter(QueqiaoVpnService.ACTION_STATE);
        registerReceiver(stateReceiver, filter, null, null, Context.RECEIVER_NOT_EXPORTED);
        startService(new Intent(this, QueqiaoVpnService.class)
                .setAction(QueqiaoVpnService.ACTION_STATUS));
    }

    @Override
    protected void onStop() {
        unregisterReceiver(stateReceiver);
        super.onStop();
    }

    @Override
    protected void onDestroy() {
        worker.shutdownNow();
        super.onDestroy();
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        handleIncomingIntent(intent);
    }

    @SuppressLint("SetTextI18n")
    private View buildShell() {
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(themeColor(android.R.attr.colorBackground));
        root.setOnApplyWindowInsetsListener((view, insets) -> {
            android.graphics.Insets bars = insets.getInsets(WindowInsets.Type.systemBars());
            view.setPadding(0, bars.top, 0, bars.bottom);
            return insets;
        });

        pageContainer = new FrameLayout(this);
        root.addView(pageContainer, new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                0,
                1));

        LinearLayout navigation = new LinearLayout(this);
        navigation.setOrientation(LinearLayout.HORIZONTAL);
        navigation.setPadding(dp(8), dp(4), dp(8), dp(6));
        navigation.setBackgroundColor(themeColor(android.R.attr.colorBackgroundFloating));
        homeTab = navigationButton("Home", Page.HOME);
        profilesTab = navigationButton("Profiles", Page.PROFILES);
        settingsTab = navigationButton("Settings", Page.SETTINGS);
        navigation.addView(homeTab, weightedWrap());
        navigation.addView(profilesTab, weightedWrap());
        navigation.addView(settingsTab, weightedWrap());
        root.addView(navigation, matchWrap());
        return root;
    }

    private Button navigationButton(String label, Page page) {
        Button button = new Button(this);
        button.setText(label);
        button.setAllCaps(false);
        button.setMinHeight(dp(52));
        button.setElevation(0);
        button.setStateListAnimator(null);
        button.setBackgroundTintList(ColorStateList.valueOf(Color.TRANSPARENT));
        button.setOnClickListener(view -> showPage(page));
        return button;
    }

    private void showPage(Page page) {
        currentPage = page;
        homeTab.setSelected(page == Page.HOME);
        profilesTab.setSelected(page == Page.PROFILES);
        settingsTab.setSelected(page == Page.SETTINGS);
        styleNavigationButton(homeTab, page == Page.HOME);
        styleNavigationButton(profilesTab, page == Page.PROFILES);
        styleNavigationButton(settingsTab, page == Page.SETTINGS);
        pageContainer.removeAllViews();
        switch (page) {
            case HOME:
                pageContainer.addView(buildHomePage());
                break;
            case PROFILES:
                pageContainer.addView(buildProfilesPage());
                break;
            case SETTINGS:
                pageContainer.addView(buildSettingsPage());
                break;
        }
    }

    @SuppressLint("SetTextI18n")
    private View buildHomePage() {
        LinearLayout content = pageContent("Queqiao", "Private access through your selected provider");

        LinearLayout connectionCard = card();
        statusView = text("Disconnected", 27, Typeface.BOLD);
        statusView.setGravity(Gravity.CENTER_HORIZONTAL);
        connectionCard.addView(statusView, matchWrap());
        connectionSubtitle = text("Import a profile to get started", 14, Typeface.NORMAL);
        connectionSubtitle.setGravity(Gravity.CENTER_HORIZONTAL);
        connectionSubtitle.setPadding(0, dp(5), 0, dp(14));
        connectionCard.addView(connectionSubtitle, matchWrap());
        connectionButton = primaryButton("Connect");
        connectionButton.setOnClickListener(view -> toggleConnection());
        connectionCard.addView(connectionButton, matchWrap());
        content.addView(connectionCard, spacedCard());

        ProfileRepository.ProfileRecord selected = selectedRecord();
        if (selected == null) {
            LinearLayout empty = card();
            empty.addView(text("No VPN profile", 19, Typeface.BOLD), matchWrap());
            TextView detail = text(
                    "Import a one-time invitation to create a device-bound profile.",
                    14,
                    Typeface.NORMAL);
            detail.setPadding(0, dp(6), 0, dp(12));
            empty.addView(detail, matchWrap());
            Button add = primaryButton("Import invitation");
            add.setOnClickListener(view -> showImportDialog(null));
            empty.addView(add, matchWrap());
            content.addView(empty, spacedCard());
        } else {
            LinearLayout current = card();
            current.addView(sectionTitle("Current connection"), matchWrap());
            addLabelValue(current, "Profile", selected.displayName);
            addLabelValue(current, "Provider", selected.summary.endpoint);
            addLabelValue(current, "Traffic policy", selected.trafficPolicy.title);
            // This is status information, deliberately rendered as text rather than a button.
            addLabelValue(current, "Active device", selected.summary.deviceName);
            Button manage = secondaryButton("Manage profiles");
            manage.setOnClickListener(view -> showPage(Page.PROFILES));
            current.addView(manage, topSpaced());
            content.addView(current, spacedCard());

            LinearLayout metrics = card();
            metrics.addView(sectionTitle("This connection"), matchWrap());
            LinearLayout row = new LinearLayout(this);
            row.setOrientation(LinearLayout.HORIZONTAL);
            downloadedView = metric("Downloaded", formatBytes(bytesDown));
            uploadedView = metric("Uploaded", formatBytes(bytesUp));
            flowsView = metric("Active flows", Long.toString(activeFlows));
            row.addView(downloadedView, weightedWrap());
            row.addView(uploadedView, weightedWrap());
            row.addView(flowsView, weightedWrap());
            metrics.addView(row, matchWrap());
            content.addView(metrics, spacedCard());
        }

        TextView privacy = text(
                "Your provider can observe destinations, timing, and traffic that is not protected end-to-end.",
                12,
                Typeface.NORMAL);
        privacy.setPadding(dp(6), dp(4), dp(6), dp(20));
        content.addView(privacy, matchWrap());
        renderConnectionState();
        return scroll(content);
    }

    @SuppressLint("SetTextI18n")
    private View buildProfilesPage() {
        LinearLayout content = pageContent("Profiles", "Choose the identity and provider used by the VPN");
        Button add = primaryButton("Import invitation");
        add.setOnClickListener(view -> showImportDialog(null));
        content.addView(add, spacedCard());

        if (repository.hasEnrollmentDraft()) {
            LinearLayout pending = card();
            pending.addView(text("Pending enrollment", 17, Typeface.BOLD), matchWrap());
            pending.addView(text(
                    "Resume with the original device key before importing another invitation.",
                    14,
                    Typeface.NORMAL), matchWrap());
            Button resume = secondaryButton("Resume import");
            resume.setOnClickListener(view -> showImportDialog(null));
            pending.addView(resume, topSpaced());
            content.addView(pending, spacedCard());
        }

        if (catalog.profiles.isEmpty()) {
            TextView empty = text("No Queqiao profiles have been imported.", 15, Typeface.NORMAL);
            empty.setGravity(Gravity.CENTER);
            empty.setPadding(dp(20), dp(42), dp(20), dp(42));
            content.addView(empty, matchWrap());
        } else {
            for (ProfileRepository.ProfileRecord profile : catalog.profiles) {
                Button row = new Button(this);
                boolean selected = profile.id.equals(catalog.selectedProfileId);
                String marker = selected ? "ACTIVE  ·  " : "";
                row.setText(marker + profile.displayName + "\n"
                        + profile.summary.endpoint + "\nDevice: " + profile.summary.deviceName);
                row.setGravity(Gravity.START | Gravity.CENTER_VERTICAL);
                row.setAllCaps(false);
                row.setTextSize(15);
                row.setPadding(dp(16), dp(13), dp(16), dp(13));
                row.setOnClickListener(view -> showProfileDialog(profile.id));
                row.setContentDescription(
                        profile.displayName + (selected ? ", active profile" : ", available profile"));
                content.addView(row, spacedCard());
            }
        }
        if (isTunnelActive()) {
            TextView locked = text(
                    "Disconnect before switching or editing profiles.",
                    13,
                    Typeface.BOLD);
            locked.setPadding(dp(8), dp(8), dp(8), dp(16));
            content.addView(locked, matchWrap());
        }
        return scroll(content);
    }

    private View buildSettingsPage() {
        LinearLayout content = pageContent("Settings", "Privacy, security, and application information");

        LinearLayout privacy = card();
        privacy.addView(sectionTitle("Traffic and privacy"), matchWrap());
        addBodyText(privacy, "No ads or analytics.");
        addBodyText(privacy, "Aggregate connection counters remain in memory only.");
        addBodyText(
                privacy,
                "The active provider can observe destinations, timing, sizes, and content that is not protected end-to-end.");
        content.addView(privacy, spacedCard());

        LinearLayout security = card();
        security.addView(sectionTitle("Profile security"), matchWrap());
        addBodyText(security, "Device keys are encrypted by Android Keystore and excluded from backup.");
        addBodyText(
                security,
                "Queqiao imports one-time invitations instead of portable private profile files. Deleting a profile requires a new invitation.");
        content.addView(security, spacedCard());

        LinearLayout about = card();
        about.addView(sectionTitle("About"), matchWrap());
        addLabelValue(about, "Version", applicationVersion());
        Button licenses = secondaryButton("Open-source licenses");
        licenses.setOnClickListener(view -> showLicenses());
        about.addView(licenses, topSpaced());
        Button systemSettings = secondaryButton("Open Android VPN settings");
        systemSettings.setOnClickListener(view -> openVpnSettings());
        about.addView(systemSettings, topSpaced());
        content.addView(about, spacedCard());
        return scroll(content);
    }

    private void toggleConnection() {
        if (isTunnelActive()) {
            disconnect();
        } else {
            prepareConnection();
        }
    }

    private void prepareConnection() {
        if (selectedRecord() == null) {
            showImportDialog(null);
            return;
        }
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(
                    new String[]{Manifest.permission.POST_NOTIFICATIONS},
                    REQUEST_NOTIFICATIONS);
            return;
        }
        prepareConnectionAfterNotificationPermission();
    }

    private void prepareConnectionAfterNotificationPermission() {
        ProfileRepository.ProfileRecord selected = selectedRecord();
        if (selected == null) {
            showImportDialog(null);
            return;
        }
        setBusy(true, "Validating profile…");
        worker.execute(() -> {
            try {
                ProfileRepository.ActiveProfile active = repository.profile(selected.id);
                if (active == null) {
                    throw new GeneralSecurityException("The selected Queqiao profile no longer exists");
                }
                String profile = active.profileJson;
                if (Mobilecore.profileNeedsRenewal(profile) != 0) {
                    runOnUiThread(() -> updateStatusText("Renewing identity…", null));
                    profile = Mobilecore.renewProfile(profile);
                    repository.replaceProfile(selected.id, profile);
                }
                pendingConnectProfileId = selected.id;
                runOnUiThread(this::requestVpnPermission);
            } catch (Exception exception) {
                showFailure("Cannot connect", exception);
            }
        });
    }

    private void requestVpnPermission() {
        Intent permission = VpnService.prepare(this);
        if (permission == null) {
            startVpnService();
        } else {
            startActivityForResult(permission, REQUEST_VPN);
        }
    }

    @Override
    protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode != REQUEST_VPN) {
            return;
        }
        if (resultCode == RESULT_OK) {
            startVpnService();
        } else {
            setBusy(false, "VPN permission was not granted");
        }
    }

    @Override
    public void onRequestPermissionsResult(
            int requestCode,
            String[] permissions,
            int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode == REQUEST_NOTIFICATIONS) {
            // Android permits the VPN foreground service even when notification
            // permission is declined; the system still surfaces it in Task Manager.
            prepareConnectionAfterNotificationPermission();
        }
    }

    private void startVpnService() {
        String profileId = pendingConnectProfileId;
        pendingConnectProfileId = null;
        if (profileId == null) {
            showFailure(
                    "Cannot connect",
                    new GeneralSecurityException("The selected Queqiao profile is unavailable"));
            return;
        }
        Intent intent = new Intent(this, QueqiaoVpnService.class)
                .setAction(QueqiaoVpnService.ACTION_CONNECT)
                .putExtra(QueqiaoVpnService.EXTRA_PROFILE_ID, profileId);
        startForegroundService(intent);
        tunnelState = Mobilecore.StateStarting;
        serviceProfileId = profileId;
        busy = true;
        renderConnectionState();
    }

    private void disconnect() {
        Intent intent = new Intent(this, QueqiaoVpnService.class)
                .setAction(QueqiaoVpnService.ACTION_DISCONNECT);
        startService(intent);
        tunnelState = Mobilecore.StateStopping;
        busy = true;
        renderConnectionState();
    }

    @SuppressLint("SetTextI18n")
    private void showImportDialog(String suppliedInvitation) {
        boolean hasDraft = repository.hasEnrollmentDraft();
        LinearLayout content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        content.setPadding(dp(20), dp(8), dp(20), 0);

        EditText invitation = new EditText(this);
        EditText deviceName = new EditText(this);
        if (hasDraft) {
            content.addView(text(
                    "An enrollment is ready to resume with its original device key.",
                    14,
                    Typeface.NORMAL), matchWrap());
        } else {
            invitation.setHint("queqiao:// one-time invitation");
            invitation.setText(suppliedInvitation == null ? "" : suppliedInvitation);
            invitation.setInputType(
                    InputType.TYPE_CLASS_TEXT
                            | InputType.TYPE_TEXT_FLAG_MULTI_LINE
                            | InputType.TYPE_TEXT_VARIATION_URI);
            invitation.setMinLines(4);
            invitation.setGravity(Gravity.TOP | Gravity.START);
            content.addView(invitation, matchWrap());

            Button paste = secondaryButton("Paste invitation");
            paste.setOnClickListener(view -> pasteInvitation(invitation));
            content.addView(paste, topSpaced());

            deviceName.setHint("Device name");
            deviceName.setText(Build.MODEL);
            deviceName.setSelectAllOnFocus(true);
            deviceName.setInputType(
                    InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_CAP_SENTENCES);
            content.addView(deviceName, topSpaced());
        }

        AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle(hasDraft ? "Resume profile import" : "Import profile")
                .setView(content)
                .setNegativeButton("Cancel", null)
                .setNeutralButton(hasDraft ? "Discard pending" : null, null)
                .setPositiveButton(hasDraft ? "Resume" : "Import", null)
                .create();
        dialog.setOnShowListener(ignored -> {
            dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener(view -> {
                String invitationText = invitation.getText().toString().trim();
                String requestedDeviceName = deviceName.getText().toString().trim();
                if (!hasDraft && (invitationText.isEmpty() || requestedDeviceName.isEmpty())) {
                    invitation.setError(invitationText.isEmpty() ? "Invitation is required" : null);
                    deviceName.setError(requestedDeviceName.isEmpty() ? "Device name is required" : null);
                    return;
                }
                dialog.getButton(AlertDialog.BUTTON_POSITIVE).setEnabled(false);
                enrollOrResume(invitationText, requestedDeviceName, dialog);
            });
            if (hasDraft) {
                dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setTextColor(Color.RED);
                dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setOnClickListener(view ->
                        confirmDiscardEnrollment(dialog));
            }
        });
        dialog.show();
    }

    private void enrollOrResume(String invitation, String deviceName, AlertDialog dialog) {
        setBusy(true, repository.hasEnrollmentDraft() ? "Resuming import…" : "Importing profile…");
        worker.execute(() -> {
            try {
                String draft = repository.enrollmentDraft();
                if (draft == null) {
                    Mobilecore.validateInvitation(invitation);
                    draft = Mobilecore.prepareEnrollment(invitation, deviceName);
                    repository.saveEnrollmentDraft(draft);
                }
                String profile = Mobilecore.completeEnrollment(draft);
                repository.importProfile(profile);
                repository.discardEnrollmentDraft();
                runOnUiThread(() -> {
                    dialog.dismiss();
                    setBusy(false, "Profile imported");
                    refreshCatalog();
                });
            } catch (Exception exception) {
                runOnUiThread(() -> dialog.getButton(AlertDialog.BUTTON_POSITIVE).setEnabled(true));
                showFailure("Profile import failed", exception);
            }
        });
    }

    private void confirmDiscardEnrollment(AlertDialog parent) {
        new AlertDialog.Builder(this)
                .setTitle("Discard pending enrollment?")
                .setMessage(
                        "The pending device key will be deleted. The invitation may already be consumed and might not be reusable.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Discard", (dialog, which) -> worker.execute(() -> {
                    try {
                        repository.discardEnrollmentDraft();
                        runOnUiThread(() -> {
                            parent.dismiss();
                            refreshCatalog();
                        });
                    } catch (Exception exception) {
                        showFailure("Could not discard enrollment", exception);
                    }
                }))
                .show();
    }

    @SuppressLint("SetTextI18n")
    private void showProfileDialog(String profileId) {
        ProfileRepository.ProfileRecord profile = catalog.find(profileId);
        if (profile == null) {
            refreshCatalog();
            return;
        }
        boolean active = profile.id.equals(catalog.selectedProfileId);
        boolean editable = !isTunnelActive() && !busy;

        LinearLayout content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        content.setPadding(dp(20), dp(4), dp(20), 0);
        addLabelValue(content, "Status", active ? "Active profile" : "Available");
        addLabelValue(content, "Provider", profile.summary.name);
        addLabelValue(content, "Endpoint", profile.summary.endpoint);
        addLabelValue(content, "Active device", profile.summary.deviceName);
        addLabelValue(content, "Certificate expires", formatExpiry(profile.summary.certificateExpiry));

        TextView policyTitle = sectionTitle("Traffic policy");
        policyTitle.setPadding(0, dp(18), 0, dp(2));
        content.addView(policyTitle, matchWrap());
        RadioGroup policies = new RadioGroup(this);
        for (TrafficPolicy policy : TrafficPolicy.values()) {
            RadioButton option = new RadioButton(this);
            option.setId(View.generateViewId());
            option.setText(policy.title + "\n" + policy.detail);
            option.setTextSize(14);
            option.setTag(policy);
            option.setChecked(profile.trafficPolicy == policy);
            option.setEnabled(editable);
            policies.addView(option, matchWrap());
        }
        policies.setOnCheckedChangeListener((group, checkedId) -> {
            RadioButton option = group.findViewById(checkedId);
            if (option != null && option.getTag() instanceof TrafficPolicy) {
                updateTrafficPolicy(profile.id, (TrafficPolicy) option.getTag());
            }
        });
        content.addView(policies, matchWrap());

        AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle(profile.displayName)
                .setView(scroll(content))
                .setNegativeButton("Close", null)
                .setNeutralButton("Delete", null)
                .setPositiveButton(active ? "Rename" : "Use profile", null)
                .create();
        dialog.setOnShowListener(ignored -> {
            dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setTextColor(Color.RED);
            dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setEnabled(editable);
            dialog.getButton(AlertDialog.BUTTON_NEUTRAL).setOnClickListener(view ->
                    confirmDeleteProfile(profile, dialog));
            dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener(view -> {
                if (active) {
                    showRenameDialog(profile, dialog);
                } else if (editable) {
                    selectProfile(profile.id, dialog);
                }
            });
            dialog.getButton(AlertDialog.BUTTON_POSITIVE).setEnabled(editable);
        });
        dialog.show();
    }

    private void selectProfile(String profileId, AlertDialog dialog) {
        worker.execute(() -> {
            try {
                repository.select(profileId);
                runOnUiThread(() -> {
                    dialog.dismiss();
                    refreshCatalog();
                });
            } catch (Exception exception) {
                showFailure("Could not select profile", exception);
            }
        });
    }

    private void updateTrafficPolicy(String profileId, TrafficPolicy policy) {
        if (isTunnelActive()) {
            showFailure(
                    "Disconnect first",
                    new IllegalStateException("Disconnect before changing the traffic policy"));
            return;
        }
        worker.execute(() -> {
            try {
                repository.setTrafficPolicy(profileId, policy);
                runOnUiThread(this::refreshCatalog);
            } catch (Exception exception) {
                showFailure("Could not update traffic policy", exception);
            }
        });
    }

    private void showRenameDialog(ProfileRepository.ProfileRecord profile, AlertDialog parent) {
        EditText name = new EditText(this);
        name.setText(profile.displayName);
        name.setSelectAllOnFocus(true);
        int padding = dp(20);
        name.setPadding(padding, dp(8), padding, dp(8));
        AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle("Rename profile")
                .setView(name)
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Save", null)
                .create();
        dialog.setOnShowListener(ignored ->
                dialog.getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener(view -> {
                    String requested = name.getText().toString().trim();
                    if (requested.isEmpty()) {
                        name.setError("Profile name is required");
                        return;
                    }
                    worker.execute(() -> {
                        try {
                            repository.rename(profile.id, requested);
                            runOnUiThread(() -> {
                                dialog.dismiss();
                                parent.dismiss();
                                refreshCatalog();
                            });
                        } catch (Exception exception) {
                            showFailure("Could not rename profile", exception);
                        }
                    });
                }));
        dialog.show();
    }

    private void confirmDeleteProfile(ProfileRepository.ProfileRecord profile, AlertDialog parent) {
        new AlertDialog.Builder(this)
                .setTitle("Delete " + profile.displayName + "?")
                .setMessage(
                        "This permanently deletes the device key. A new invitation is required to enroll again.")
                .setNegativeButton("Cancel", null)
                .setPositiveButton("Delete", (dialog, which) -> worker.execute(() -> {
                    try {
                        repository.delete(profile.id);
                        runOnUiThread(() -> {
                            parent.dismiss();
                            refreshCatalog();
                        });
                    } catch (Exception exception) {
                        showFailure("Could not delete profile", exception);
                    }
                }))
                .show();
    }

    private void refreshCatalog() {
        worker.execute(() -> {
            try {
                ProfileRepository.Catalog refreshed = repository.catalog();
                runOnUiThread(() -> {
                    catalog = refreshed;
                    showPage(currentPage);
                });
            } catch (Exception exception) {
                showFailure("Stored profiles are unavailable", exception);
            }
        });
    }

    private void renderConnectionState() {
        if (statusView == null || connectionButton == null || connectionSubtitle == null) {
            return;
        }
        String display = displayState(tunnelState);
        updateStatusText(display, tunnelMessage);
        ProfileRepository.ProfileRecord selected = selectedRecord();
        if (selected == null) {
            connectionSubtitle.setText(R.string.import_profile_intro);
        } else {
            connectionSubtitle.setText(getString(
                    R.string.connection_profile_summary,
                    selected.displayName,
                    selected.summary.endpoint));
        }
        boolean active = isTunnelActive();
        connectionButton.setText(active ? "Disconnect" : "Connect");
        boolean connectionEnabled = !busy && (active || selected != null);
        connectionButton.setEnabled(connectionEnabled);
        connectionButton.setBackgroundTintList(ColorStateList.valueOf(
                !connectionEnabled
                        ? Color.rgb(158, 158, 158)
                        : active
                        ? Color.rgb(183, 28, 28)
                        : themeColor(android.R.attr.colorAccent)));
        if (downloadedView != null) {
            downloadedView.setText(getString(
                    R.string.downloaded_metric,
                    formatBytes(bytesDown)));
            uploadedView.setText(getString(
                    R.string.uploaded_metric,
                    formatBytes(bytesUp)));
            flowsView.setText(getString(R.string.active_flows_metric, activeFlows));
        }
    }

    private void updateStatusText(String state, String message) {
        if (statusView != null) {
            statusView.setText(message == null || message.isBlank() ? state : state + "\n" + message);
        }
    }

    private void setBusy(boolean value, String message) {
        busy = value;
        tunnelMessage = message;
        renderConnectionState();
    }

    private void parseMetrics(String encoded) {
        if (encoded == null) {
            if (!isTunnelActive()) {
                bytesUp = 0;
                bytesDown = 0;
                activeFlows = 0;
            }
            return;
        }
        try {
            JSONObject transport = new JSONObject(encoded).optJSONObject("transport");
            if (transport != null) {
                bytesUp = transport.optLong("BytesUp", 0);
                bytesDown = transport.optLong("BytesDown", 0);
                activeFlows = transport.optLong("ActiveFlows", 0);
            }
        } catch (Exception ignored) {
            // Metrics are optional UI decoration and never affect tunnel state.
        }
    }

    private void handleIncomingIntent(Intent intent) {
        if (intent == null) {
            return;
        }
        String invitation = null;
        if (Intent.ACTION_VIEW.equals(intent.getAction())) {
            Uri data = intent.getData();
            if (data != null && "queqiao".equalsIgnoreCase(data.getScheme())) {
                invitation = data.toString();
            }
        } else if (Intent.ACTION_SEND.equals(intent.getAction())
                && "text/plain".equals(intent.getType())) {
            invitation = intent.getStringExtra(Intent.EXTRA_TEXT);
        }
        if (invitation != null && invitation.trim().startsWith("queqiao://")) {
            showImportDialog(invitation.trim());
        }
    }

    private void pasteInvitation(EditText destination) {
        ClipboardManager clipboard = getSystemService(ClipboardManager.class);
        if (!clipboard.hasPrimaryClip()) {
            destination.setError("The clipboard is empty");
            return;
        }
        ClipData clip = clipboard.getPrimaryClip();
        if (clip == null || clip.getItemCount() == 0) {
            destination.setError("The clipboard is empty");
            return;
        }
        CharSequence value = clip.getItemAt(0).coerceToText(this);
        destination.setText(value == null ? "" : value.toString().trim());
    }

    private void openVpnSettings() {
        try {
            startActivity(new Intent(Settings.ACTION_VPN_SETTINGS));
        } catch (Exception exception) {
            showFailure("VPN settings are unavailable", exception);
        }
    }

    private void showLicenses() {
        try {
            String notices;
            try (java.io.InputStream input = getAssets().open("THIRD_PARTY_NOTICES.txt")) {
                ByteArrayOutputStream output = new ByteArrayOutputStream(64 * 1024);
                byte[] buffer = new byte[8 * 1024];
                int total = 0;
                int count;
                while ((count = input.read(buffer)) != -1) {
                    total += count;
                    if (total > 1024 * 1024) {
                        throw new IOException("License notice exceeds 1 MiB");
                    }
                    output.write(buffer, 0, count);
                }
                notices = output.toString(StandardCharsets.UTF_8.name());
            }
            TextView text = text(notices, 11, Typeface.MONOSPACE.getStyle());
            text.setTextIsSelectable(true);
            text.setTypeface(Typeface.MONOSPACE);
            text.setPadding(dp(16), dp(16), dp(16), dp(16));
            new AlertDialog.Builder(this)
                    .setTitle("Open-source licenses")
                    .setView(scroll(text))
                    .setPositiveButton("Close", null)
                    .show();
        } catch (IOException exception) {
            showFailure("License notices are unavailable", exception);
        }
    }

    private void showFailure(String title, Exception exception) {
        String message = exception.getMessage();
        if (message == null || message.isBlank()) {
            message = exception.getClass().getSimpleName();
        }
        String finalMessage = message;
        runOnUiThread(() -> {
            busy = false;
            new AlertDialog.Builder(this)
                    .setTitle(title)
                    .setMessage(finalMessage)
                    .setPositiveButton("OK", null)
                    .show();
            renderConnectionState();
        });
    }

    private LinearLayout pageContent(String title, String subtitle) {
        LinearLayout content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        content.setPadding(dp(18), dp(16), dp(18), dp(24));
        content.addView(text(title, 30, Typeface.BOLD), matchWrap());
        TextView subtitleView = text(subtitle, 14, Typeface.NORMAL);
        subtitleView.setPadding(0, dp(3), 0, dp(12));
        content.addView(subtitleView, matchWrap());
        return content;
    }

    private LinearLayout card() {
        LinearLayout card = new LinearLayout(this);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setPadding(dp(17), dp(17), dp(17), dp(17));
        GradientDrawable background = new GradientDrawable();
        background.setColor(themeColor(android.R.attr.colorBackgroundFloating));
        background.setCornerRadius(dp(18));
        card.setBackground(background);
        card.setElevation(dp(1));
        return card;
    }

    private TextView text(String value, float size, int style) {
        TextView text = new TextView(this);
        text.setText(value);
        text.setTextSize(size);
        text.setTypeface(Typeface.DEFAULT, style);
        return text;
    }

    private TextView sectionTitle(String value) {
        TextView title = text(value, 18, Typeface.BOLD);
        title.setPadding(0, 0, 0, dp(8));
        return title;
    }

    private TextView metric(String label, String value) {
        TextView metric = text(label + "\n" + value, 13, Typeface.NORMAL);
        metric.setPadding(dp(3), dp(8), dp(3), dp(3));
        metric.setGravity(Gravity.START);
        metric.setMaxLines(2);
        return metric;
    }

    private void addLabelValue(LinearLayout parent, String label, String value) {
        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setPadding(0, dp(6), 0, dp(6));
        TextView labelView = text(label, 14, Typeface.NORMAL);
        TextView valueView = text(value, 14, Typeface.BOLD);
        valueView.setGravity(Gravity.END);
        valueView.setTextIsSelectable(true);
        row.addView(labelView, weightedWrap());
        row.addView(valueView, weightedWrap());
        parent.addView(row, matchWrap());
    }

    private void addBodyText(LinearLayout parent, String body) {
        TextView text = text(body, 14, Typeface.NORMAL);
        text.setPadding(0, dp(5), 0, dp(5));
        parent.addView(text, matchWrap());
    }

    private Button primaryButton(String label) {
        Button button = new Button(this);
        button.setText(label);
        button.setAllCaps(false);
        button.setTextSize(16);
        button.setTextColor(Color.WHITE);
        button.setMinHeight(dp(52));
        button.setBackgroundTintList(ColorStateList.valueOf(themeColor(android.R.attr.colorAccent)));
        return button;
    }

    private Button secondaryButton(String label) {
        Button button = new Button(this);
        button.setText(label);
        button.setAllCaps(false);
        button.setMinHeight(dp(48));
        return button;
    }

    private void styleNavigationButton(Button button, boolean selected) {
        button.setTextColor(selected
                ? themeColor(android.R.attr.colorAccent)
                : themeColor(android.R.attr.textColorSecondary));
        button.setTypeface(Typeface.DEFAULT, selected ? Typeface.BOLD : Typeface.NORMAL);
    }

    private ProfileRepository.ProfileRecord selectedRecord() {
        return catalog.find(catalog.selectedProfileId);
    }

    private boolean isTunnelActive() {
        return Mobilecore.StateRunning.equals(tunnelState)
                || Mobilecore.StateStarting.equals(tunnelState)
                || Mobilecore.StateStopping.equals(tunnelState);
    }

    private boolean isTransitioning() {
        return Mobilecore.StateStarting.equals(tunnelState)
                || Mobilecore.StateStopping.equals(tunnelState);
    }

    private String displayState(String state) {
        if (state == null || state.isEmpty()) {
            return "Unavailable";
        }
        if (Mobilecore.StateStopped.equals(state)) {
            return "Disconnected";
        }
        if (Mobilecore.StateRunning.equals(state)) {
            return "Connected";
        }
        return Character.toUpperCase(state.charAt(0)) + state.substring(1) + "…";
    }

    private static String valueOr(String value, String fallback) {
        return value == null || value.isBlank() ? fallback : value;
    }

    private String formatBytes(long bytes) {
        if (bytes < 1024) {
            return bytes + " B";
        }
        double value = bytes;
        String[] units = {"KiB", "MiB", "GiB", "TiB"};
        int unit = -1;
        do {
            value /= 1024.0;
            unit++;
        } while (value >= 1024 && unit < units.length - 1);
        return String.format(java.util.Locale.getDefault(), "%.1f %s", value, units[unit]);
    }

    private String formatExpiry(String encoded) {
        try {
            Date date = Date.from(Instant.parse(encoded));
            return DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(date);
        } catch (Exception ignored) {
            return encoded;
        }
    }

    private String applicationVersion() {
        try {
            android.content.pm.PackageInfo info = getPackageManager().getPackageInfo(getPackageName(), 0);
            return info.versionName + " (" + info.getLongVersionCode() + ")";
        } catch (PackageManager.NameNotFoundException exception) {
            return "Unknown";
        }
    }

    private int themeColor(int attribute) {
        android.util.TypedValue value = new android.util.TypedValue();
        getTheme().resolveAttribute(attribute, value, true);
        if (value.resourceId != 0) {
            return getResources().getColorStateList(value.resourceId, getTheme()).getDefaultColor();
        }
        return value.data;
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }

    private ScrollView scroll(View child) {
        ScrollView scroll = new ScrollView(this);
        scroll.setFillViewport(true);
        scroll.addView(child);
        return scroll;
    }

    private LinearLayout.LayoutParams topSpaced() {
        LinearLayout.LayoutParams params = matchWrap();
        params.topMargin = dp(8);
        return params;
    }

    private LinearLayout.LayoutParams spacedCard() {
        LinearLayout.LayoutParams params = matchWrap();
        params.topMargin = dp(8);
        params.bottomMargin = dp(8);
        return params;
    }

    private static LinearLayout.LayoutParams matchWrap() {
        return new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.WRAP_CONTENT);
    }

    private static LinearLayout.LayoutParams weightedWrap() {
        return new LinearLayout.LayoutParams(
                0,
                LinearLayout.LayoutParams.WRAP_CONTENT,
                1);
    }
}
