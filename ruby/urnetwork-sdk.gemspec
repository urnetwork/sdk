Gem::Specification.new do |spec|
  spec.name = "urnetwork-sdk"
  spec.version = "0.0.1.pre.dev.0"
  spec.summary = "URnetwork userspace networking SDK"
  spec.authors = ["URnetwork"]
  spec.email = ["support@ur.io"]
  spec.homepage = "https://ur.io"
  spec.license = "MPL-2.0"
  spec.required_ruby_version = ">= 2.6"
  spec.files = Dir["lib/**/*", "README.md", "LICENSE"]
  spec.require_paths = ["lib"]
  spec.platform = Gem::Platform.new(ENV.fetch("SDK_GEM_PLATFORM", "ruby"))
  spec.add_dependency "ffi", ">= 1.17", "< 2"
  spec.metadata = {"source_code_uri" => "https://github.com/urnetwork/sdk", "documentation_uri" => "https://ur.io/docs/getting-started-sdk"}
end
