gem "urnetwork-sdk", ENV.fetch("SDK_GEM_VERSION")
require "urnetwork"
raise "ABI" unless URnetwork::Raw.urnet_abi_version == 1
raise "string" unless URnetwork.take_string(URnetwork::Raw.urnet_new_id).size == 36
before = URnetwork::Raw.urnet_live_handle_count
raise "UTF-8" unless URnetwork.take_string(URnetwork::Raw.urnet_new_network_space_key("héllo", "main")).include?("héllo")
URnetwork::Handle.open(URnetwork::Raw.urnet_new_network_space_manager_no_storage) do |manager|
  URnetwork::Raw.urnet_network_space_manager_close(manager.handle)
end
raise "handle leak" unless URnetwork::Raw.urnet_live_handle_count == before
puts "Ruby package: #{URnetwork.version}"
