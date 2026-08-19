package io.github.bojieli.queqiao;

import android.app.Activity;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.util.TypedValue;
import android.view.Gravity;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

/**
 * The view vocabulary shared by the activity and the connection controllers.
 * This app builds every screen in code, so without one kit each controller
 * would reinvent the card, the button, and the density conversion, and the two
 * build variants would drift apart visually.
 */
final class UiKit {
    private final Activity activity;

    UiKit(Activity activity) {
        this.activity = activity;
    }

    LinearLayout card() {
        LinearLayout card = new LinearLayout(activity);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setPadding(dp(17), dp(17), dp(17), dp(17));
        GradientDrawable background = new GradientDrawable();
        background.setColor(themeColor(android.R.attr.colorBackgroundFloating));
        background.setCornerRadius(dp(18));
        card.setBackground(background);
        card.setElevation(dp(1));
        return card;
    }

    TextView text(String value, float size, int style) {
        TextView text = new TextView(activity);
        text.setText(value);
        text.setTextSize(size);
        text.setTypeface(Typeface.DEFAULT, style);
        return text;
    }

    TextView sectionTitle(String value) {
        TextView title = text(value, 18, Typeface.BOLD);
        title.setPadding(0, 0, 0, dp(8));
        return title;
    }

    TextView metric(String label, String value) {
        TextView metric = text(label + "\n" + value, 13, Typeface.NORMAL);
        metric.setPadding(dp(3), dp(8), dp(3), dp(3));
        metric.setGravity(Gravity.START);
        metric.setMaxLines(2);
        return metric;
    }

    void addLabelValue(LinearLayout parent, String label, String value) {
        LinearLayout row = new LinearLayout(activity);
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

    void addBodyText(LinearLayout parent, String body) {
        TextView text = text(body, 14, Typeface.NORMAL);
        text.setPadding(0, dp(5), 0, dp(5));
        parent.addView(text, matchWrap());
    }

    /**
     * Renders configuration a user has to copy verbatim into another app. It is
     * selectable and monospaced because a mistyped port silently produces a
     * connection that never carries traffic.
     */
    TextView codeBlock(String body) {
        TextView code = text(body, 12, Typeface.NORMAL);
        code.setTypeface(Typeface.MONOSPACE);
        code.setTextIsSelectable(true);
        code.setPadding(dp(12), dp(10), dp(12), dp(10));
        GradientDrawable background = new GradientDrawable();
        background.setColor(themeColor(android.R.attr.colorBackground));
        background.setCornerRadius(dp(10));
        code.setBackground(background);
        return code;
    }

    Button primaryButton(String label) {
        Button button = new Button(activity);
        button.setText(label);
        button.setAllCaps(false);
        button.setTextSize(16);
        button.setTextColor(Color.WHITE);
        button.setMinHeight(dp(52));
        button.setBackgroundTintList(ColorStateList.valueOf(themeColor(android.R.attr.colorAccent)));
        return button;
    }

    Button secondaryButton(String label) {
        Button button = new Button(activity);
        button.setText(label);
        button.setAllCaps(false);
        button.setMinHeight(dp(48));
        return button;
    }

    ScrollView scroll(View child) {
        ScrollView scroll = new ScrollView(activity);
        scroll.setFillViewport(true);
        scroll.addView(child);
        return scroll;
    }

    int themeColor(int attribute) {
        TypedValue value = new TypedValue();
        activity.getTheme().resolveAttribute(attribute, value, true);
        if (value.resourceId != 0) {
            return activity.getResources()
                    .getColorStateList(value.resourceId, activity.getTheme())
                    .getDefaultColor();
        }
        return value.data;
    }

    int dp(int value) {
        return Math.round(value * activity.getResources().getDisplayMetrics().density);
    }

    LinearLayout.LayoutParams topSpaced() {
        LinearLayout.LayoutParams params = matchWrap();
        params.topMargin = dp(8);
        return params;
    }

    LinearLayout.LayoutParams spacedCard() {
        LinearLayout.LayoutParams params = matchWrap();
        params.topMargin = dp(8);
        params.bottomMargin = dp(8);
        return params;
    }

    static LinearLayout.LayoutParams matchWrap() {
        return new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.WRAP_CONTENT);
    }

    static LinearLayout.LayoutParams weightedWrap() {
        return new LinearLayout.LayoutParams(
                0,
                LinearLayout.LayoutParams.WRAP_CONTENT,
                1);
    }
}
