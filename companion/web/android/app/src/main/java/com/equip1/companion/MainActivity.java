package com.equip1.companion;

import android.graphics.Color;
import android.os.Bundle;
import android.webkit.WebSettings;
import androidx.core.view.WindowCompat;
import androidx.core.view.WindowInsetsControllerCompat;
import com.getcapacitor.BridgeActivity;

public class MainActivity extends BridgeActivity {
    // Matches web/src/components/ui/tokens.css --bg. Target SDK 35+ enforces
    // edge-to-edge and ignores android:statusBarColor/navigationBarColor —
    // the status/nav bar areas just show whatever's actually drawn behind
    // them, which for a WebView is the WebView's own background color. Left
    // at the WebView default (white), the system bar insets render as a
    // stark white/black mismatch against the app's dark canvas until the
    // page's own CSS background paints over them a frame later.
    private static final int APP_BG = Color.parseColor("#0b0b0b");

    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        // Allow the https://localhost WebView bundle to fetch plain HTTP from
        // the device API on the local network (Mixed Content).
        getBridge().getWebView().getSettings()
            .setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
        getBridge().getWebView().setBackgroundColor(APP_BG);
        getWindow().getDecorView().setBackgroundColor(APP_BG);

        // Under edge-to-edge the bar backgrounds are the WebView (dark), so the
        // status/nav glyphs must be *light* to be visible. Appearance flags are
        // still honored on SDK 35+ even though bar colors are not. false = not
        // light-appearance = light icons on a dark bar.
        WindowInsetsControllerCompat insets =
            WindowCompat.getInsetsController(getWindow(), getWindow().getDecorView());
        insets.setAppearanceLightStatusBars(false);
        insets.setAppearanceLightNavigationBars(false);
    }
}
