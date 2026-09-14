import io.ur.sdk.Sdk;
class Smoke {
    public static void main(String[] args) throws Exception {
        if (Sdk.raw.urnet_abi_version() != 1) throw new Exception("ABI");
        if (Sdk.takeString(Sdk.raw.urnet_new_id()).length() != 36) throw new Exception("string");
        long before = Sdk.raw.urnet_live_handle_count();
        if (!Sdk.takeString(Sdk.raw.urnet_new_network_space_key("héllo", "main")).contains("héllo")) throw new Exception("UTF-8");
        try (var manager = new Sdk.Handle(Sdk.raw.urnet_new_network_space_manager_no_storage())) {
            Sdk.raw.urnet_network_space_manager_close(manager.handle());
        }
        if (Sdk.raw.urnet_live_handle_count() != before) throw new Exception("handle leak");
        System.out.println("Java package: " + Sdk.version());
    }
}
