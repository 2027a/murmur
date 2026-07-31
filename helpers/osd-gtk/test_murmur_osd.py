import importlib.util
import pathlib
import sys
import types
import unittest


class FakeGLib:
    IO_IN = 1
    IO_HUP = 2
    IO_ERR = 4
    next_source = 1
    timers = {}

    @classmethod
    def timeout_add(cls, timeout, callback):
        source = cls.next_source
        cls.next_source += 1
        cls.timers[source] = (timeout, callback)
        return source

    @classmethod
    def source_remove(cls, source):
        cls.timers.pop(source, None)


class FakeGtk:
    quit_calls = 0

    @classmethod
    def main_quit(cls):
        cls.quit_calls += 1


gi = types.ModuleType("gi")
gi.require_version = lambda *_args: None
repository = types.ModuleType("gi.repository")
repository.GLib = FakeGLib
repository.Gtk = FakeGtk
repository.GtkLayerShell = object()
sys.modules.setdefault("gi", gi)
sys.modules.setdefault("gi.repository", repository)

helper_path = pathlib.Path(__file__).with_name("murmur-osd.py")
spec = importlib.util.spec_from_file_location("murmur_osd", helper_path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class FakeWindow:
    def __init__(self):
        self.visible = False

    def show_all(self):
        self.visible = True

    def hide(self):
        self.visible = False


class FakeLabel:
    def __init__(self):
        self.text = ""

    def set_text(self, text):
        self.text = text


def new_osd():
    osd = module.MurmurOSD.__new__(module.MurmurOSD)
    osd.success_timeout_ms = 17
    osd.error_timeout_ms = 29
    osd.input_closed = False
    osd.terminal_received = False
    osd.hide_source = 0
    osd.pulse_source = 0
    osd.pulse_on = False
    osd.window = FakeWindow()
    osd.label = FakeLabel()
    osd.set_css = lambda _accent: None
    osd.set_recording_pulse = lambda _enabled: None
    return osd


class HelperShutdownTest(unittest.TestCase):
    def setUp(self):
        FakeGLib.timers = {}
        FakeGLib.next_source = 1
        FakeGtk.quit_calls = 0

    def test_eof_waits_for_terminal_timeout(self):
        osd = new_osd()
        osd.apply_payload(
            {"v": 1, "state": "terminal", "visible": True, "result": "completed"}
        )
        self.assertTrue(osd.window.visible)
        timeout, callback = FakeGLib.timers[osd.hide_source]
        self.assertEqual(timeout, 17)

        osd.finish_input()
        self.assertEqual(FakeGtk.quit_calls, 0)
        self.assertTrue(osd.window.visible)

        self.assertFalse(callback())
        self.assertFalse(osd.window.visible)
        self.assertEqual(FakeGtk.quit_calls, 1)

    def test_eof_without_terminal_quits_immediately(self):
        osd = new_osd()
        osd.finish_input()
        self.assertEqual(FakeGtk.quit_calls, 1)


if __name__ == "__main__":
    unittest.main()
