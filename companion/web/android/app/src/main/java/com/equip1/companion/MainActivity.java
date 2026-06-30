package com.equip1.companion;

import android.os.Bundle;
import android.webkit.WebSettings;
import com.getcapacitor.BridgeActivity;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        // Allow the https://localhost WebView bundle to fetch plain HTTP from
        // the device API on the local network (Mixed Content).
        getBridge().getWebView().getSettings()
            .setMixedContentMode(WebSettings.MIXED_CONTENT_ALWAYS_ALLOW);
    }
}
