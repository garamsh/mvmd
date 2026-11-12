//! The boot: what mvmd's description sets going, in the order `guest.md` gives, and the state
//! the rest of the service reads to know whether the guest is ready.

use std::io;
use std::sync::Arc;

use tokio::sync::watch;

use crate::description::{BootDescription, CatchUp, NetworkConfig};
use crate::error::{BootStep, Error, Unbooted};
use crate::exec::Execs;
use crate::system::{Disks, Resources, System};
use crate::uuid::Filesystem;
use crate::v1alpha1::{ExecRequest, ExecStatus};

/// The exec ID the entrypoint runs under.
pub const ENTRYPOINT: &str = "entrypoint";

/// A boot step that failed.
#[derive(Debug, Clone)]
pub(crate) struct Failure {
    pub(crate) step: BootStep,
    pub(crate) detail: String,
}

impl Failure {
    fn new(step: BootStep, cause: impl std::fmt::Display) -> Self {
        Self {
            step,
            detail: cause.to_string(),
        }
    }

    /// The line the console carries for it.
    pub(crate) fn console_line(&self) -> String {
        format!(
            "mvmd-guest-agent: boot failed step={} detail={}",
            self.step, self.detail
        )
    }
}

impl From<Failure> for Error {
    fn from(failure: Failure) -> Self {
        Self::Boot {
            step: failure.step,
            detail: failure.detail,
        }
    }
}

/// Where the boot stands.
#[derive(Debug, Clone)]
enum Phase {
    /// The agent does not boot the guest: it only serves.
    Disabled,
    /// Waiting for mvmd's description.
    Pending,
    /// A description arrived and its steps are running.
    Booting,
    /// Every step succeeded.
    Booted(Arc<BootDescription>),
    /// A step failed; the guest stays as that step left it.
    Failed(Failure),
}

/// The boot of one guest.
pub(crate) struct Boot {
    phase: watch::Sender<Phase>,
    /// What the steps act on; `None` when the agent does not boot the guest.
    system: Option<System>,
    execs: Arc<Execs>,
    /// The restore ID of the last catch-up that succeeded. It is held while a catch-up runs, so
    /// two calls with one ID do the work once.
    last_restore: tokio::sync::Mutex<Option<String>>,
}

impl Boot {
    /// A boot waiting for mvmd's description.
    pub(crate) fn pending(system: System, execs: Arc<Execs>) -> Self {
        Self {
            phase: watch::Sender::new(Phase::Pending),
            system: Some(system),
            execs,
            last_restore: tokio::sync::Mutex::new(None),
        }
    }

    /// No boot: the agent serves from the start and refuses to be described.
    pub(crate) fn disabled(execs: Arc<Execs>) -> Self {
        Self {
            phase: watch::Sender::new(Phase::Disabled),
            system: None,
            execs,
            last_restore: tokio::sync::Mutex::new(None),
        }
    }

    /// Whether the calls that act on the guest may be served.
    pub(crate) fn ready(&self) -> Result<(), Error> {
        match &*self.phase.borrow() {
            Phase::Disabled | Phase::Booted(_) => Ok(()),
            Phase::Pending | Phase::Booting => Err(Error::NotBooted {
                state: Unbooted::Pending,
                reason: "the guest has not booted: Boot has not succeeded".into(),
            }),
            Phase::Failed(Failure { step, detail }) => Err(Error::NotBooted {
                state: Unbooted::Failed,
                reason: format!("the guest did not boot: boot step {step}: {detail}"),
            }),
        }
    }

    /// The disks of this guest, or why there are none: the agent that only serves the channel is
    /// not a guest, and its `/sys` is not one's.
    pub(crate) fn disks(&self) -> Result<Arc<dyn Disks>, Error> {
        self.system
            .as_ref()
            .map(|system| system.disks.clone())
            .ok_or_else(|| {
                Error::PreconditionFailed(
                    "this agent is not the guest's init and has no disks".into(),
                )
            })
    }

    /// The resources of this guest, or why there are none, for the reason `disks` gives.
    pub(crate) fn resources(&self) -> Result<Arc<dyn Resources>, Error> {
        self.system
            .as_ref()
            .map(|system| system.resources.clone())
            .ok_or_else(|| {
                Error::PreconditionFailed(
                    "this agent is not the guest's init and has no vCPUs to bring online".into(),
                )
            })
    }

    /// The refusal of a call that needs a booted guest, or `None` when the guest has booted.
    fn unbooted(&self, reason: &str) -> Option<Error> {
        let state = match &*self.phase.borrow() {
            Phase::Booted(_) => return None,
            Phase::Failed(_) => Unbooted::Failed,
            Phase::Disabled | Phase::Pending | Phase::Booting => Unbooted::Pending,
        };
        Some(Error::NotBooted {
            state,
            reason: reason.into(),
        })
    }

    /// Resolves once every step has succeeded; never, if one fails.
    pub(crate) async fn booted(&self) {
        let mut phase = self.phase.subscribe();
        // The sender lives as long as `self`, so the wait ends only on success.
        let _ = phase
            .wait_for(|phase| matches!(phase, Phase::Booted(_)))
            .await;
    }

    /// Start the boot for `description`, or find what an earlier call to it came to, and wait
    /// for it to settle. A boot that starts sets the clock to `host_unix_nanos` first; a repeat
    /// carries a time that is ignored.
    ///
    /// The steps run in a task of their own, which settles the phase whatever becomes of this
    /// call: a client that gives up, or a connection that resets, leaves the boot running and a
    /// repeat finds its outcome.
    pub(crate) async fn boot(
        self: &Arc<Self>,
        description: BootDescription,
        host_unix_nanos: Option<i64>,
    ) -> Result<(), Error> {
        let Some(system) = self.system.clone() else {
            return Err(Error::PreconditionFailed(
                "this agent does not boot the guest".into(),
            ));
        };
        let claimed = self.phase.send_if_modified(|phase| {
            let pending = matches!(phase, Phase::Pending);
            if pending {
                *phase = Phase::Booting;
            }
            pending
        });
        if claimed {
            let boot = self.clone();
            let description = description.clone();
            tokio::spawn(async move {
                let settled = match boot.steps(&system, &description, host_unix_nanos).await {
                    Ok(()) => Phase::Booted(Arc::new(description)),
                    Err(failure) => {
                        system.console.line(&failure.console_line());
                        Phase::Failed(failure)
                    }
                };
                boot.phase.send_replace(settled);
            });
        }
        let settled = self
            .phase
            .subscribe()
            .wait_for(|phase| !matches!(phase, Phase::Booting))
            .await
            .map(|phase| phase.clone())
            .map_err(|_| Error::PreconditionFailed("the boot was dropped".into()))?;
        match settled {
            Phase::Booted(done) if *done == description => Ok(()),
            Phase::Booted(_) => Err(Error::PreconditionFailed(
                "the guest booted with a different description".into(),
            )),
            Phase::Failed(failure) => Err(failure.into()),
            Phase::Disabled | Phase::Pending | Phase::Booting => {
                Err(Error::PreconditionFailed("the boot did not settle".into()))
            }
        }
    }

    /// Replace the Machine's network configuration, once the guest has booted.
    pub(crate) async fn update_network(&self, config: NetworkConfig) -> Result<(), Error> {
        let Some(system) = &self.system else {
            return Err(Error::PreconditionFailed(
                "this agent does not boot the guest".into(),
            ));
        };
        if let Some(refusal) =
            self.unbooted("the network is configured by Boot, which has not succeeded")
        {
            return Err(refusal);
        }
        let network = system.network.clone();
        Ok(blocking(BootStep::Network, move || network.apply(&config)).await?)
    }

    /// Catch a restored guest up: entropy, then the clock, then the network. A call with the
    /// restore ID of the last one that succeeded has nothing left to do.
    ///
    /// A call dropped part-way leaves the ID unrecorded, so a repeat does every step again,
    /// each of which is safe to do twice.
    pub(crate) async fn catch_up(&self, request: CatchUp) -> Result<(), Error> {
        let Some(system) = &self.system else {
            return Err(Error::PreconditionFailed(
                "this agent does not boot the guest".into(),
            ));
        };
        if let Some(refusal) = self.unbooted("the guest has not booted: Boot has not succeeded") {
            return Err(refusal);
        }
        let mut last = self.last_restore.lock().await;
        if last.as_deref() == Some(request.restore_id.as_str()) {
            return Ok(());
        }
        let entropy = system.entropy.clone();
        let bytes = request.entropy;
        io_step("mix entropy into the kernel's pool", move || {
            entropy.mix_and_reseed(&bytes)
        })
        .await?;
        let clock = system.clock.clone();
        let nanos = request.host_unix_nanos;
        io_step("set the wall clock", move || clock.set_realtime(nanos)).await?;
        let network = system.network.clone();
        let config = request.network;
        io_step("configure the network", move || network.apply(&config)).await?;
        *last = Some(request.restore_id);
        Ok(())
    }

    /// The steps of the boot, each reported by its own name when it fails.
    async fn steps(
        &self,
        system: &System,
        description: &BootDescription,
        host_unix_nanos: Option<i64>,
    ) -> Result<(), Failure> {
        if let Some(nanos) = host_unix_nanos {
            let clock = system.clock.clone();
            blocking(BootStep::Clock, move || clock.set_realtime(nanos)).await?;
        }
        let mounts = system.mounts.clone();
        let (image, root_disk) = (description.image, description.root_disk);
        let (image_device, root_device) = blocking(BootStep::Check, move || {
            Ok((
                mounts.find_device(Filesystem::Erofs, &image)?,
                mounts.find_device(Filesystem::Ext4, &root_disk)?,
            ))
        })
        .await?;

        let mounts = system.mounts.clone();
        blocking(BootStep::MountImage, move || {
            mounts.mount_image(&image_device)
        })
        .await?;
        let mounts = system.mounts.clone();
        blocking(BootStep::MountRootDisk, move || {
            mounts.mount_root_disk(&root_device)
        })
        .await?;
        let mounts = system.mounts.clone();
        blocking(BootStep::Overlay, move || mounts.overlay()).await?;
        let mounts = system.mounts.clone();
        blocking(BootStep::SwitchRoot, move || mounts.switch_root()).await?;

        let network = system.network.clone();
        let config = description.network.clone();
        blocking(BootStep::Network, move || network.apply(&config)).await?;

        if !description.files.is_empty() {
            let mounts = system.mounts.clone();
            let files = description.files.clone();
            blocking(BootStep::PlaceFiles, move || mounts.place_files(&files)).await?;
        }

        let disks = system.disks.clone();
        blocking(BootStep::MountDisks, move || {
            disks.mount_recorded().map_err(io::Error::other)
        })
        .await?;

        let run = &description.run;
        let identity = match run.user.clone() {
            Some(spec) => {
                let execs = self.execs.clone();
                Some(blocking(BootStep::ApplyUser, move || execs.identity(&spec)).await?)
            }
            None => None,
        };
        let record = self
            .execs
            .start(
                ExecRequest {
                    exec_id: ENTRYPOINT.into(),
                    argv: run.argv.clone(),
                    env: run.env.clone(),
                    working_dir: run.working_dir.clone(),
                    user: String::new(),
                },
                identity.as_ref(),
            )
            .map_err(|failed| Failure::new(BootStep::Entrypoint, failed))?;
        if record.status() == ExecStatus::NotStarted {
            return Err(Failure::new(BootStep::Entrypoint, record.reason));
        }
        Ok(())
    }
}

/// Run a blocking step of a catch-up off the async threads, naming it if it fails.
async fn io_step(
    context: &str,
    work: impl FnOnce() -> io::Result<()> + Send + 'static,
) -> Result<(), Error> {
    let failed = |source| Error::Io {
        context: context.to_owned(),
        source,
    };
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|joined| failed(io::Error::other(joined)))?
        .map_err(failed)
}

/// Run a blocking step off the async threads, naming it if it fails.
async fn blocking<T: Send + 'static>(
    step: BootStep,
    work: impl FnOnce() -> io::Result<T> + Send + 'static,
) -> Result<T, Failure> {
    tokio::task::spawn_blocking(work)
        .await
        .map_err(|joined| Failure::new(step, joined))?
        .map_err(|failed| Failure::new(step, failed))
}
