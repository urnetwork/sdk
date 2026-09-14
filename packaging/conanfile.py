# Conan 2 requires Python recipes. Shared preparation and publication use Go.
from conan import ConanFile
from conan.errors import ConanInvalidConfiguration
from conan.tools.files import get, copy, load, check_sha256, unzip
import json
import os

class URnetworkSdk(ConanFile):
    name = "urnetwork-sdk"
    package_type = "shared-library"
    settings = "os", "arch"
    options = {"cpp": [True, False]}
    default_options = {"cpp": False}
    license = "MPL-2.0"
    url = "https://github.com/urnetwork/sdk"
    description = "URnetwork C ABI and C++ interfaces"
    exports_sources = "native.json"
    required_conan_version = ">=2.12"

    def set_version(self):
        self.version = json.loads(load(self, os.path.join(self.recipe_folder, "native.json")))["version"]

    def requirements(self):
        if self.options.cpp:
            self.requires("nlohmann_json/3.12.0", transitive_headers=True)

    def package_id(self):
        # Both headers ship together. The option adds the JSON dependency;
        # it does not change the C ABI runtime bytes.
        self.info.options.clear()

    def build(self):
        system = {"Macos": "darwin", "Linux": "linux", "Windows": "windows"}.get(str(self.settings.os))
        arch = {"x86_64": "amd64", "armv8": "arm64"}.get(str(self.settings.arch))
        index = json.loads(load(self, os.path.join(self.source_folder, "native.json")))
        asset = next((a for a in index["assets"] if a["platform"] == f"{system}-{arch}"), None)
        if asset is None:
            raise ConanInvalidConfiguration("This release has no runtime for this OS/architecture")
        cache = os.environ.get("SDK_CONAN_ASSET_CACHE")
        if cache:
            archive = os.path.join(cache, asset["archive"])
            check_sha256(self, archive, asset["sha256"])
            unzip(self, archive, destination="runtime")
        else:
            get(self, asset["url"], sha256=asset["sha256"], destination="runtime", strip_root=False)

    def package(self):
        copy(self, "*", src=os.path.join(self.build_folder, "runtime"), dst=self.package_folder)

    def package_info(self):
        self.cpp_info.libs = ["URnetworkSdk"]
        self.cpp_info.set_property("cmake_file_name", "urnetwork-sdk")
        self.cpp_info.set_property("cmake_target_name", "urnetwork::sdk")
