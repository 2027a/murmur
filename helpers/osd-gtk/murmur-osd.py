#!/usr/bin/env python3

import argparse
import json
import os
import sys
import time

import gi

gi.require_version("Gtk", "3.0")
gi.require_version("GtkLayerShell", "0.1")

from gi.repository import GLib, Gtk, GtkLayerShell  # type: ignore


MAX_INPUT_BUFFER = 64 * 1024
DEFAULT_ICON = "audio-input-microphone-symbolic"

LABELS = {
    "starting": "Demarrage",
    "recording": "Ecoute",
    "stopping": "Arret",
    "transcribing": "Transcription",
    "processing": "Traitement",
    "delivering": "Livraison",
    "error": "Erreur",
}

RESULT_LABELS = {
    "completed": "Dictee terminee",
    "cancelled": "Dictee annulee",
    "no_speech": "Aucune parole detectee",
    "failed": "Echec de la dictee",
}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Murmur GTK3 layer-shell OSD")
    parser.add_argument(
        "--position",
        choices=("top-left", "top-center", "center"),
        default="top-center",
    )
    parser.add_argument("--style", choices=("nord", "compact"), default="nord")
    parser.add_argument("--success-timeout-ms", type=int, default=2500)
    parser.add_argument("--error-timeout-ms", type=int, default=4000)
    parser.add_argument("--check", action="store_true")
    return parser.parse_args()


class MurmurOSD:
    def __init__(
        self,
        position: str,
        style: str,
        success_timeout_ms: int,
        error_timeout_ms: int,
    ) -> None:
        self.position = position
        self.style = style
        self.success_timeout_ms = max(0, success_timeout_ms)
        self.error_timeout_ms = max(0, error_timeout_ms)
        self.input_buffer = b""
        self.hide_source = 0
        self.pulse_source = 0
        self.pulse_on = False

        self.window = Gtk.Window(type=Gtk.WindowType.TOPLEVEL)
        self.window.set_decorated(False)
        self.window.set_resizable(False)
        self.window.set_app_paintable(True)
        self.window.set_accept_focus(False)
        self.window.set_focus_on_map(False)
        self.window.set_skip_taskbar_hint(True)
        self.window.set_skip_pager_hint(True)
        self.window.stick()

        GtkLayerShell.init_for_window(self.window)
        GtkLayerShell.set_layer(self.window, GtkLayerShell.Layer.OVERLAY)
        GtkLayerShell.set_keyboard_mode(
            self.window, GtkLayerShell.KeyboardMode.NONE
        )
        GtkLayerShell.set_exclusive_zone(self.window, 0)
        self.set_position()

        spacing = 12 if style == "compact" else 20
        border = 14 if style == "compact" else 24
        self.frame = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=spacing)
        self.frame.get_style_context().add_class("murmur-osd")
        self.frame.set_border_width(border)
        self.frame.set_size_request(240 if style == "compact" else 320, -1)

        self.image = Gtk.Image.new_from_icon_name(DEFAULT_ICON, Gtk.IconSize.DIALOG)
        self.image.set_pixel_size(48 if style == "compact" else 72)
        self.label = Gtk.Label(label="Murmur")
        self.label.set_xalign(0.0)
        self.label.get_style_context().add_class("murmur-osd-label")
        self.frame.pack_start(self.image, False, False, 0)
        self.frame.pack_start(self.label, False, False, 0)
        self.window.add(self.frame)

        self.css = Gtk.CssProvider()
        Gtk.StyleContext.add_provider_for_screen(
            self.window.get_screen(),
            self.css,
            Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION,
        )
        self.set_css("#88C0D0")
        self.window.connect("destroy", Gtk.main_quit)

        os.set_blocking(sys.stdin.fileno(), False)
        GLib.io_add_watch(
            sys.stdin,
            GLib.IO_IN | GLib.IO_HUP | GLib.IO_ERR,
            self.read_stdin,
        )

    def set_position(self) -> None:
        for edge in (
            GtkLayerShell.Edge.TOP,
            GtkLayerShell.Edge.BOTTOM,
            GtkLayerShell.Edge.LEFT,
            GtkLayerShell.Edge.RIGHT,
        ):
            GtkLayerShell.set_anchor(self.window, edge, False)

        if self.position == "top-left":
            GtkLayerShell.set_anchor(self.window, GtkLayerShell.Edge.TOP, True)
            GtkLayerShell.set_anchor(self.window, GtkLayerShell.Edge.LEFT, True)
            self.window.set_halign(Gtk.Align.START)
            self.window.set_valign(Gtk.Align.START)
        elif self.position == "top-center":
            GtkLayerShell.set_anchor(self.window, GtkLayerShell.Edge.TOP, True)
            GtkLayerShell.set_anchor(self.window, GtkLayerShell.Edge.LEFT, True)
            GtkLayerShell.set_anchor(self.window, GtkLayerShell.Edge.RIGHT, True)
            self.window.set_halign(Gtk.Align.CENTER)
            self.window.set_valign(Gtk.Align.START)
        else:
            for edge in (
                GtkLayerShell.Edge.TOP,
                GtkLayerShell.Edge.BOTTOM,
                GtkLayerShell.Edge.LEFT,
                GtkLayerShell.Edge.RIGHT,
            ):
                GtkLayerShell.set_anchor(self.window, edge, True)
            self.window.set_halign(Gtk.Align.CENTER)
            self.window.set_valign(Gtk.Align.CENTER)

    def set_css(self, accent: str) -> None:
        font_size = 20 if self.style == "compact" else 26
        css = f"""
        .murmur-osd {{
            background: rgba(59, 66, 82, 0.94);
            border: 3px solid {accent};
            border-radius: 16px;
            color: #ECEFF4;
            padding: 12px 18px;
        }}
        .murmur-osd-label {{
            font-size: {font_size}px;
            font-weight: 700;
        }}
        """
        self.css.load_from_data(css.encode())

    def read_stdin(self, _source: object, condition: int) -> bool:
        try:
            chunk = os.read(sys.stdin.fileno(), 16384)
        except BlockingIOError:
            return True
        except OSError:
            Gtk.main_quit()
            return False

        if chunk:
            self.input_buffer += chunk
            if len(self.input_buffer) > MAX_INPUT_BUFFER:
                self.input_buffer = b""
            while b"\n" in self.input_buffer:
                line, self.input_buffer = self.input_buffer.split(b"\n", 1)
                self.apply_line(line)
        if not chunk or condition & (GLib.IO_HUP | GLib.IO_ERR):
            Gtk.main_quit()
            return False
        return True

    def apply_line(self, line: bytes) -> None:
        try:
            payload = json.loads(line.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            return
        if not isinstance(payload, dict) or payload.get("v") != 1:
            return
        self.apply_payload(payload)

    def apply_payload(self, payload: dict) -> None:
        if not payload.get("visible", True):
            self.cancel_timers()
            self.window.hide()
            return

        state = str(payload.get("state", ""))
        result = str(payload.get("result", ""))
        label = RESULT_LABELS.get(result, LABELS.get(state, "Murmur"))
        partial = payload.get("partial")
        total = payload.get("total")
        if isinstance(partial, int) and isinstance(total, int) and partial > 0 and total > 0:
            label = f"{label} {partial}/{total}"
        self.label.set_text(label)

        error = state == "error" or result == "failed"
        self.set_css("#BF616A" if error else "#A3BE8C")
        self.set_recording_pulse(state == "recording")
        self.window.show_all()

        self.cancel_hide_timer()
        if state == "terminal" or error:
            timeout = self.error_timeout_ms if error else self.success_timeout_ms
            self.hide_source = GLib.timeout_add(timeout, self.hide)

    def set_recording_pulse(self, enabled: bool) -> None:
        if not enabled:
            self.cancel_pulse_timer()
        elif not self.pulse_source:
            self.pulse_source = GLib.timeout_add(500, self.pulse)

    def pulse(self) -> bool:
        self.pulse_on = not self.pulse_on
        self.set_css("#88C0D0" if self.pulse_on else "#A3BE8C")
        return True

    def hide(self) -> bool:
        self.window.hide()
        self.hide_source = 0
        self.cancel_pulse_timer()
        return False

    def cancel_hide_timer(self) -> None:
        if self.hide_source:
            GLib.source_remove(self.hide_source)
            self.hide_source = 0

    def cancel_pulse_timer(self) -> None:
        if self.pulse_source:
            GLib.source_remove(self.pulse_source)
            self.pulse_source = 0
            self.pulse_on = False

    def cancel_timers(self) -> None:
        self.cancel_hide_timer()
        self.cancel_pulse_timer()


def main() -> int:
    args = parse_args()
    if args.check:
        return 0
    if os.environ.get("WAYLAND_DISPLAY") is None:
        print("murmur-osd: WAYLAND_DISPLAY is not set", file=sys.stderr)
        return 1

    MurmurOSD(
        args.position,
        args.style,
        args.success_timeout_ms,
        args.error_timeout_ms,
    )
    Gtk.main()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
