package com.florious95.corral.console

import android.Manifest
import android.app.Activity
import android.content.ClipData
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.MediaStore
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat
import androidx.core.content.FileProvider
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.io.File

class MainActivity : FlutterActivity() {
    private val channelName = "com.florious95.corral.console/app"
    private val chooserRequest = 4101
    private val cameraRequest = 4102
    private val cameraPermissionRequest = 4103
    private val mediaPermissionRequest = 4104
    private var pendingChooserResult: MethodChannel.Result? = null
    private var pendingChooserCall: MethodCall? = null
    private var pendingCameraUri: Uri? = null

    private val normalPreferences by lazy {
        getSharedPreferences("app_settings", MODE_PRIVATE)
    }

    private val securePreferences by lazy {
        val masterKey = MasterKey.Builder(this)
            .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
            .build()
        EncryptedSharedPreferences.create(
            this,
            "secure_settings",
            masterKey,
            EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
            EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
        )
    }

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, channelName)
            .setMethodCallHandler { call, result ->
                when (call.method) {
                    "loadSettings" -> result.success(
                        mapOf(
                            "authKey" to securePreferences.getString("auth_key", null),
                            "gatewayOverride" to normalPreferences.getString("gateway_override", null),
                            "gatewayChoiceMade" to normalPreferences.getBoolean("gateway_choice_made", false),
                            "proxyPort" to normalPreferences.getInt("proxy_port", 17878),
                            "lastWebRoute" to normalPreferences.getString("last_web_route", "/"),
                            "stateDir" to File(filesDir, "corral").absolutePath,
                        ),
                    )
                    "saveAuthKey" -> {
                        securePreferences.edit()
                            .putString("auth_key", call.argument<String>("authKey"))
                            .apply()
                        result.success(null)
                    }
                    "saveGatewayOverride" -> {
                        val address = call.argument<String>("address")
                        normalPreferences.edit()
                            .putBoolean("gateway_choice_made", true)
                            .apply {
                                if (address == null) remove("gateway_override")
                                else putString("gateway_override", address)
                            }
                            .apply()
                        result.success(null)
                    }
                    "saveProxyPort" -> {
                        normalPreferences.edit()
                            .putInt("proxy_port", call.argument<Int>("port") ?: 17878)
                            .apply()
                        result.success(null)
                    }
                    "saveLastWebRoute" -> {
                        normalPreferences.edit()
                            .putString("last_web_route", call.argument<String>("route") ?: "/")
                            .apply()
                        result.success(null)
                    }
                    "clearSettings" -> {
                        securePreferences.edit().clear().apply()
                        normalPreferences.edit().clear().apply()
                        result.success(null)
                    }
                    "showFileChooser" -> showFileChooser(call, result)
                    else -> result.notImplemented()
                }
            }
    }

    private fun showFileChooser(call: MethodCall, result: MethodChannel.Result) {
        pendingChooserResult?.success(emptyList<String>())
        pendingChooserResult = result
        pendingChooserCall = call
        val capture = call.argument<Boolean>("capture") == true
        if (capture && acceptsImages(call)) {
            if (hasPermission(Manifest.permission.CAMERA)) launchCamera()
            else ActivityCompat.requestPermissions(
                this,
                arrayOf(Manifest.permission.CAMERA),
                cameraPermissionRequest,
            )
            return
        }

        val permission = mediaReadPermission()
        if (permission != null && !hasPermission(permission)) {
            ActivityCompat.requestPermissions(this, arrayOf(permission), mediaPermissionRequest)
        } else {
            launchDocumentPicker()
        }
    }

    private fun acceptsImages(call: MethodCall): Boolean {
        val types = call.argument<List<String>>("acceptTypes") ?: emptyList()
        return types.isEmpty() || types.any { it.isEmpty() || it == "*/*" || it.startsWith("image/") }
    }

    private fun launchDocumentPicker() {
        val call = pendingChooserCall ?: return finishChooser(emptyList())
        val types = (call.argument<List<String>>("acceptTypes") ?: emptyList())
            .filter { it.isNotBlank() }
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            type = if (types.size == 1) types.first() else "*/*"
            if (types.size > 1) putExtra(Intent.EXTRA_MIME_TYPES, types.toTypedArray())
            putExtra(Intent.EXTRA_ALLOW_MULTIPLE, call.argument<Boolean>("multiple") == true)
        }
        startActivityForResult(intent, chooserRequest)
    }

    private fun launchCamera() {
        val directory = File(cacheDir, "camera").apply { mkdirs() }
        val file = File.createTempFile("capture_", ".jpg", directory)
        val uri = FileProvider.getUriForFile(this, "$packageName.fileprovider", file)
        pendingCameraUri = uri
        val intent = Intent(MediaStore.ACTION_IMAGE_CAPTURE).apply {
            putExtra(MediaStore.EXTRA_OUTPUT, uri)
            clipData = ClipData.newRawUri("capture", uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION)
        }
        if (intent.resolveActivity(packageManager) == null) finishChooser(emptyList())
        else startActivityForResult(intent, cameraRequest)
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        when (requestCode) {
            cameraRequest -> finishChooser(
                if (resultCode == Activity.RESULT_OK) listOfNotNull(pendingCameraUri?.toString())
                else emptyList(),
            )
            chooserRequest -> {
                if (resultCode != Activity.RESULT_OK) return finishChooser(emptyList())
                val uris = mutableListOf<String>()
                data?.clipData?.let { clip ->
                    for (index in 0 until clip.itemCount) uris.add(clip.getItemAt(index).uri.toString())
                }
                data?.data?.let { uri ->
                    if (uris.isEmpty()) uris.add(uri.toString())
                    runCatching {
                        contentResolver.takePersistableUriPermission(
                            uri,
                            Intent.FLAG_GRANT_READ_URI_PERMISSION,
                        )
                    }
                }
                finishChooser(uris)
            }
        }
    }

    override fun onRequestPermissionsResult(
        requestCode: Int,
        permissions: Array<out String>,
        grantResults: IntArray,
    ) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        when (requestCode) {
            cameraPermissionRequest -> {
                if (grantResults.firstOrNull() == PackageManager.PERMISSION_GRANTED) launchCamera()
                else finishChooser(emptyList())
            }
            mediaPermissionRequest -> launchDocumentPicker()
        }
    }

    private fun mediaReadPermission(): String? = when {
        Build.VERSION.SDK_INT >= 33 -> Manifest.permission.READ_MEDIA_IMAGES
        Build.VERSION.SDK_INT >= 23 -> Manifest.permission.READ_EXTERNAL_STORAGE
        else -> null
    }

    private fun hasPermission(permission: String): Boolean =
        ContextCompat.checkSelfPermission(this, permission) == PackageManager.PERMISSION_GRANTED

    private fun finishChooser(uris: List<String>) {
        pendingChooserResult?.success(uris)
        pendingChooserResult = null
        pendingChooserCall = null
        pendingCameraUri = null
    }
}
