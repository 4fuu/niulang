# gomobile uses reflection and JNI entry points generated in the binding AAR.
-keep class go.** { *; }
-keep class mobilecore.** { *; }
-keepclasseswithmembers,includedescriptorclasses class * {
    native <methods>;
}
