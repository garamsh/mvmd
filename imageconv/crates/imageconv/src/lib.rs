//! The image converter: the service of `mvmd.convert.v1alpha1` served over any incoming
//! connection stream, and the duties of the converter microVM's init. It applies an image's
//! layers to one EROFS, one `mkfs.erofs` run each (ADR 0006). Production hands it vsock; tests
//! hand it a Unix socket.

mod convert;
mod error;
mod init;
mod media;
mod mkfs;
mod output;
mod plan;
mod rewrite;
mod service;
mod system;
mod tar;
mod uuid;

pub use convert::{Layers, Parameters, Pass, PassKind, convert};
pub use error::{Error, Step};
pub use init::{end_on_panic, fail, run};
pub use mkfs::{Error as MkfsError, Mkfs};
pub use service::{Converter, ServeError, serve};
pub use system::{Console, LinuxConsole, LinuxMounts, LinuxPower, Mounts, Power, System};

/// The generated contract: messages, the client and the server of `ConvertService`.
pub mod v1alpha1 {
    #![expect(
        clippy::allow_attributes,
        clippy::allow_attributes_without_reason,
        reason = "tonic-prost-build writes #[allow] without a reason"
    )]
    tonic::include_proto!("mvmd.convert.v1alpha1");
}
