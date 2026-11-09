//! The type URL of each status detail, checked against the contract it comes from.

use std::path::PathBuf;

use guest_agent::v1alpha1::{BootFailed, ExecOutputGone, NotBooted, WrongFileKind};
use prost::Name;

type Result<T = ()> = std::result::Result<T, Box<dyn std::error::Error>>;

/// The full name each message type reports is the one the compiled descriptor gives it, which is
/// what a client resolving the `Any` by name looks for.
#[test]
fn each_detail_type_url_is_the_full_name_in_the_compiled_descriptor() -> Result {
    let proto_root = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?).join("../../proto");
    let descriptors = protox::compile(["mvmd/guest/v1alpha1/guest.proto"], [&proto_root])?;
    let file = descriptors
        .file
        .iter()
        .find(|file| file.package() == "mvmd.guest.v1alpha1")
        .ok_or("the contract's file is not in the descriptor")?;
    let in_descriptor = |name: &str| {
        file.message_type
            .iter()
            .any(|message| format!("{}.{}", file.package(), message.name()) == name)
    };

    let urls = [
        (BootFailed::full_name(), BootFailed::type_url()),
        (ExecOutputGone::full_name(), ExecOutputGone::type_url()),
        (NotBooted::full_name(), NotBooted::type_url()),
        (WrongFileKind::full_name(), WrongFileKind::type_url()),
    ];
    for (name, url) in urls {
        assert!(
            in_descriptor(&name),
            "{name} is not a message of the contract"
        );
        assert!(
            url.ends_with(&format!("/{name}")),
            "{url} does not end in /{name}"
        );
    }
    // Control: a name that is not in the contract is not found, so the lookup can fail.
    assert!(!in_descriptor("mvmd.guest.v1alpha1.NoSuchDetail"));
    Ok(())
}
