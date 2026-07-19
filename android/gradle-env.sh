#!/usr/bin/env bash
# Source before running ./gradlew:  source android/gradle-env.sh
#
# 为什么需要:本机 macOS 系统 /usr/bin/java 是 Apple stub,
# 用户全局 JDK 是 25(Homebrew openjdk),Gradle 8.9 不兼容 JDK 25。
# 本 repo 用 JDK 17。
# local.properties 里的 org.gradle.java.home 只对 Gradle daemon 生效,
# gradlew 启动脚本本身要走 PATH 上的 java,所以还需要 JAVA_HOME。
#
# 若已装 direnv,可在 .envrc 里同样 source 本文件。

export JAVA_HOME="/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home"
export ANDROID_HOME="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-$ANDROID_HOME/ndk/android-ndk-r27c}"
export PATH="$JAVA_HOME/bin:$ANDROID_HOME/platform-tools:$ANDROID_HOME/cmdline-tools/latest/bin:$PATH"

# Go / gomobile(tsnet-wrapper 构建也用)
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
export PATH="$HOME/go/bin:$PATH"
