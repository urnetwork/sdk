"""Platform wheels contain the shared CGo runtime; source builds require Go."""
from pathlib import Path
import os
import subprocess
import shutil
import tempfile
from setuptools import setup
from setuptools.command.build_py import build_py
from wheel.bdist_wheel import bdist_wheel


class NativeBuild(build_py):
    def run(self):
        root = Path(__file__).resolve().parent
        if not list((root / "src/urnetwork/native").glob("*/*")):
            helper = root.parent / "packaging/go.mod"
            if helper.exists():
                subprocess.run(["go", "-C", str(helper.parent), "run", ".", "native", "python"], check=True)
            else:
                raise RuntimeError("This source distribution requires a staged CGo runtime. "
                                   "Use the platform wheel, or build from the SDK Git checkout with Go installed.")
        super().run()


class NativeWheel(bdist_wheel):
    def finalize_options(self):
        super().finalize_options()
        self.root_is_pure = False

    def get_tag(self):
        _, _, plat = super().get_tag()
        return "py3", "none", os.environ.get("SDK_WHEEL_PLATFORM", plat)


setup(cmdclass={"build_py": NativeBuild, "bdist_wheel": NativeWheel})
