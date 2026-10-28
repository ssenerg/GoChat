plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.compose.compiler)
}

// Pushes come through Firebase Cloud Messaging once a build has the
// settings of its Firebase app (Project settings, Your apps): as gochat.fcm.*
// Gradle properties or GOCHAT_FCM_* variables. Firebase reads them as the
// resources its own Gradle plugin would write; without them there are none,
// and the app gets no pushes.
val fcm = listOf(
    Triple("google_app_id", "appId", "GOCHAT_FCM_APP_ID"),
    Triple("google_api_key", "apiKey", "GOCHAT_FCM_API_KEY"),
    Triple("project_id", "projectId", "GOCHAT_FCM_PROJECT_ID"),
    Triple("gcm_defaultSenderId", "senderId", "GOCHAT_FCM_SENDER_ID"),
).associate { (resource, property, variable) ->
    resource to (providers.gradleProperty("gochat.fcm.$property").orNull ?: providers.environmentVariable(variable).orNull)
}

android {
    namespace = "io.github.ssenerg.gochat.android"
    compileSdk = libs.versions.android.compileSdk.get().toInt()

    defaultConfig {
        applicationId = "io.github.ssenerg.gochat"
        minSdk = libs.versions.android.minSdk.get().toInt()
        targetSdk = libs.versions.android.targetSdk.get().toInt()
        versionCode = 3
        versionName = "0.3.0"
        // Phones, and the 64-bit emulators; WebRTC's native code is large.
        ndk {
            abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64")
        }
        if (fcm.values.none { it.isNullOrBlank() }) {
            fcm.forEach { (name, value) -> resValue("string", name, value!!) }
        }
    }
    signingConfigs {
        // The same debug key on every machine and CI runner, so a newer
        // debug build installs over an older one. Never used for release.
        getByName("debug") {
            storeFile = file("debug.keystore")
            storePassword = "android"
            keyAlias = "androiddebugkey"
            keyPassword = "android"
        }
        // The release key stays out of the repository: its keystore and
        // passwords come from the environment, as a release job sets them.
        val keystore = System.getenv("GOCHAT_RELEASE_KEYSTORE")
        if (keystore != null) {
            create("release") {
                storeFile = file(keystore)
                storePassword = System.getenv("GOCHAT_RELEASE_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("GOCHAT_RELEASE_KEY_ALIAS")
                keyPassword = System.getenv("GOCHAT_RELEASE_KEY_PASSWORD")
            }
        }
    }
    buildTypes {
        debug {
            // A debug build installs beside a release build, never over it.
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Unsigned without the release key.
            signingConfig = signingConfigs.findByName("release")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        compose = true
        resValues = true
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}

dependencies {
    implementation(project(":ui"))
    implementation(libs.androidx.activity.compose)
}
