#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <lvgl.h>
#include "vars.h"

char app_version[100] = { 0 };
char system_version[100] = { 0 };
char lvgl_version[32] = { 0 };
char main_screen[32] = "home_screen";
char clock_timezone[16] = "auto";

const char *get_var_app_version() {
    return app_version;
}

const char *get_var_system_version() {
    return system_version;
}

const char *get_var_lvgl_version() {
    if (lvgl_version[0] == '\0') {
        char buf[32];
        sprintf(buf, "%d.%d.%d", LVGL_VERSION_MAJOR, LVGL_VERSION_MINOR, LVGL_VERSION_PATCH);
        
        
        strncpy(lvgl_version, buf, sizeof(lvgl_version) / sizeof(char));
        app_version[sizeof(lvgl_version) / sizeof(char) - 1] = 0;
    }
    return lvgl_version;
}

void set_var_app_version(const char *value) {
    strncpy(app_version, value, sizeof(app_version) / sizeof(char));
    app_version[sizeof(app_version) / sizeof(char) - 1] = 0;
}

void set_var_system_version(const char *value) {
    strncpy(system_version, value, sizeof(system_version) / sizeof(char));
    system_version[sizeof(system_version) / sizeof(char) - 1] = 0;
}

void set_var_lvgl_version(const char *value) {}

void set_var_main_screen(const char *value) {
    strncpy(main_screen, value, sizeof(main_screen) / sizeof(char));
    main_screen[sizeof(main_screen) / sizeof(char) - 1] = 0;
}

const char *get_var_main_screen() {
    return main_screen;
}

const char *get_var_clock_timezone() {
    return clock_timezone;
}

void set_var_clock_timezone(const char *value) {
    if (value == NULL || value[0] == '\0' || strcmp(value, "auto") == 0) {
        strncpy(clock_timezone, "auto", sizeof(clock_timezone) / sizeof(char));
        clock_timezone[sizeof(clock_timezone) / sizeof(char) - 1] = 0;
        return;
    }

    char *end = NULL;
    long offset = strtol(value, &end, 10);
    if (end == value || *end != '\0' || offset < -12 || offset > 14) {
        strncpy(clock_timezone, "auto", sizeof(clock_timezone) / sizeof(char));
        clock_timezone[sizeof(clock_timezone) / sizeof(char) - 1] = 0;
        return;
    }

    snprintf(clock_timezone, sizeof(clock_timezone), "%ld", offset);
}