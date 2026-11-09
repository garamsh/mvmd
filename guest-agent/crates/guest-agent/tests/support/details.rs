//! Reading the details of a gRPC status the way a client does: the `grpc-status-details-bin`
//! trailer is a `google.rpc.Status`, and each detail in it is an `Any` naming its message type.
//! The messages are decoded here from the wire, so a detail the agent mislabels or mis-encodes
//! fails the test that reads it.

use prost::Message;
use tonic::Status;

#[derive(Clone, PartialEq, Message)]
struct RpcStatus {
    #[prost(int32, tag = "1")]
    code: i32,
    #[prost(message, repeated, tag = "3")]
    details: Vec<Any>,
}

#[derive(Clone, PartialEq, Message)]
struct Any {
    #[prost(string, tag = "1")]
    type_url: String,
    #[prost(bytes = "vec", tag = "2")]
    value: Vec<u8>,
}

/// The full names of the message types `status` carries as details, in order.
pub fn detail_types(status: &Status) -> Vec<String> {
    if status.details().is_empty() {
        return Vec::new();
    }
    let Ok(rpc) = RpcStatus::decode(status.details()) else {
        return vec!["undecodable".into()];
    };
    assert_eq!(
        rpc.code,
        status.code() as i32,
        "the details' code is the status's"
    );
    rpc.details
        .into_iter()
        .map(|any| {
            any.type_url
                .strip_prefix("type.googleapis.com/")
                .unwrap_or(&any.type_url)
                .to_owned()
        })
        .collect()
}

/// The detail of `status` that is a `name` message, or `None` when it carries none.
pub fn detail<M: Message + Default>(status: &Status, name: &str) -> Option<M> {
    let rpc = RpcStatus::decode(status.details()).ok()?;
    let url = format!("type.googleapis.com/mvmd.guest.v1alpha1.{name}");
    let any = rpc.details.into_iter().find(|any| any.type_url == url)?;
    M::decode(any.value.as_slice()).ok()
}
