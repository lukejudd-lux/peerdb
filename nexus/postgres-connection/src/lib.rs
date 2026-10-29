use base64::Engine;
use pt::peerdb_peers::{PostgresConfig, SshConfig};
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{ClientConfig, DigitallySignedStruct, RootCertStore, SignatureScheme};
use std::fmt::Write;
use std::io;
use std::sync::Arc;
use std::time::Duration;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::net::UnixStream;
use tokio_postgres::config::SslMode;
use tokio_postgres::tls::MakeTlsConnect;
use tokio_postgres_rustls::MakeRustlsConnect;

const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);
use tokio_util::compat::FuturesAsyncReadCompatExt;

#[derive(Copy, Clone, Debug)]
struct NoCertificateVerification;

impl rustls::client::danger::ServerCertVerifier for NoCertificateVerification {
    fn verify_server_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> std::result::Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        Ok(rustls::client::danger::ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &rustls::crypto::aws_lc_rs::default_provider().signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &rustls::crypto::aws_lc_rs::default_provider().signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        rustls::crypto::aws_lc_rs::default_provider()
            .signature_verification_algorithms
            .supported_schemes()
    }
}

pub fn get_pg_connection_string(config: &PostgresConfig) -> String {
    let mut connection_string = String::from("postgres://");

    connection_string.push_str(&urlencoding::encode(&config.user));
    if !config.password.is_empty() {
        connection_string.push(':');
        connection_string.push_str(&urlencoding::encode(&config.password));
    }

    // Add the timeout as a query parameter, sslmode changes here appear to be useless
    write!(
        connection_string,
        "@{}:{}/{}?connect_timeout=15&application_name=peerdb_nexus",
        config.host,
        config.port,
        urlencoding::encode(&config.database)
    )
    .ok();

    connection_string
}

pub async fn create_tunnel(
    tcp: std::net::TcpStream,
    ssh_config: &SshConfig,
    remote_server: String,
    remote_port: u16,
) -> io::Result<(ssh2::Session, UnixStream)> {
    let mut session = ssh2::Session::new()?;
    session.set_tcp_stream(tcp);
    session.set_compress(true);
    session.handshake()?;
    if !ssh_config.password.is_empty() {
        session.userauth_password(&ssh_config.user, &ssh_config.password)?;
    }
    if !ssh_config.private_key.is_empty() {
        let private_key_bytes = base64::engine::general_purpose::STANDARD
            .decode(&ssh_config.private_key)
            .map_err(|e| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("Failed to decode private key: {e}"),
                )
            })?;
        let private_key = str::from_utf8(private_key_bytes.as_slice()).map_err(|e| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                format!("Invalid UTF-8 in private key: {e}"),
            )
        })?;
        session.userauth_pubkey_memory(&ssh_config.user, None, private_key, None)?;
    }
    if !ssh_config.host_key.is_empty() {
        let mut known_hosts = session.known_hosts()?;
        known_hosts.read_str(&ssh_config.host_key, ssh2::KnownHostFileKind::OpenSSH)?;
    }
    let (mut stream1, stream2) = tokio::net::UnixStream::pair()?;
    let channel = session.channel_direct_tcpip(remote_server.as_str(), remote_port, None)?;
    tracing::info!(
        "tunnel to {:}:{:} opened",
        remote_server.as_str(),
        remote_port
    );

    session.set_blocking(false);
    tokio::spawn(async move {
        let mut channel_stream = futures_util::io::AllowStdIo::new(channel.stream(0)).compat();
        loop {
            if let Err(err) = tokio::io::copy_bidirectional(&mut stream1, &mut channel_stream).await
            {
                if err.kind() == io::ErrorKind::WouldBlock {
                    tokio::time::sleep(std::time::Duration::new(0, 123456789)).await;
                    continue;
                }
                tracing::error!(
                    "tunnel to {:}:{:} failed: {:}",
                    remote_server.as_str(),
                    remote_port,
                    err
                );
            }
            break;
        }
    });

    Ok((session, stream2))
}

/// Whether the peer asks for a verified TLS connection, matching flow's Postgres connector:
/// require_tls or a root CA turns verification on, unless TLS is explicitly disabled.
fn verifies_tls(config: &PostgresConfig) -> bool {
    (config.require_tls || config.root_ca.is_some()) && config.disable_tls != Some(true)
}

/// The name the server certificate must match: tls_host when set (the host may be an
/// address that isn't on the certificate), otherwise the host.
fn tls_server_name(config: &PostgresConfig) -> &str {
    let tls_host = config.tls_host.trim();
    if tls_host.is_empty() {
        &config.host
    } else {
        tls_host
    }
}

/// TLS settings for a verified connection: the peer's root CA (or the Mozilla roots when it
/// has none), TLS 1.2 or later, and no verification only when skip_cert_verification is set.
fn verified_tls_config(config: &PostgresConfig) -> anyhow::Result<ClientConfig> {
    let mut roots = RootCertStore::empty();
    match config.root_ca.as_deref() {
        Some(pem) if !pem.trim().is_empty() => {
            for cert in CertificateDer::pem_slice_iter(pem.as_bytes()) {
                roots.add(cert.map_err(|e| anyhow::anyhow!("failed to parse root CA: {e}"))?)?;
            }
            if roots.is_empty() {
                anyhow::bail!("root CA has no certificates");
            }
        }
        _ => roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned()),
    }
    let mut tls_config = ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::aws_lc_rs::default_provider(),
    ))
    .with_safe_default_protocol_versions()?
    .with_root_certificates(roots)
    .with_no_client_auth();
    if config.skip_cert_verification {
        tls_config
            .dangerous()
            .set_certificate_verifier(Arc::new(NoCertificateVerification));
    }
    Ok(tls_config)
}

/// Connect over an established stream (an SSH tunnel or a TCP socket), requiring TLS and
/// verifying the server certificate against the peer's root CA and TLS host.
async fn connect_verified<S>(
    config: &PostgresConfig,
    stream: S,
) -> anyhow::Result<tokio_postgres::Client>
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let tls = MakeTlsConnect::<S>::make_tls_connect(
        &mut MakeRustlsConnect::new(verified_tls_config(config)?),
        tls_server_name(config),
    )?;
    let (client, connection) = tokio::time::timeout(
        CONNECT_TIMEOUT,
        tokio_postgres::Config::default()
            .user(&config.user)
            .password(&config.password)
            .dbname(&config.database)
            .application_name("peerdb_nexus")
            .ssl_mode(SslMode::Require)
            .connect_raw(stream, tls),
    )
    .await
    .map_err(|_| anyhow::anyhow!("timed out connecting to postgres"))?
    .map_err(|e| anyhow::anyhow!("error encountered while connecting to postgres {:?}", e))?;
    tokio::task::spawn(async move {
        if let Err(e) = connection.await {
            tracing::info!("connection error: {}", e)
        }
    });
    Ok(client)
}

pub async fn connect_postgres(
    config: &PostgresConfig,
) -> anyhow::Result<(tokio_postgres::Client, Option<ssh2::Session>)> {
    if let Some(ssh_config) = &config.ssh_config {
        let tcp = std::net::TcpStream::connect((ssh_config.host.as_str(), ssh_config.port as u16))?;
        tcp.set_nodelay(true)?;
        let (session, stream) =
            create_tunnel(tcp, ssh_config, config.host.clone(), config.port as u16).await?;
        if verifies_tls(config) {
            return Ok((connect_verified(config, stream).await?, Some(session)));
        }
        let (client, connection) = tokio_postgres::Config::default()
            .user(&config.user)
            .password(&config.password)
            .dbname(&config.database)
            .application_name("peerdb_nexus")
            .connect_raw(stream, tokio_postgres::NoTls)
            .await?;
        tokio::task::spawn(async move {
            if let Err(e) = connection.await {
                tracing::info!("connection error: {}", e)
            }
        });
        Ok((client, Some(session)))
    } else if verifies_tls(config) {
        let tcp = tokio::time::timeout(
            CONNECT_TIMEOUT,
            tokio::net::TcpStream::connect((config.host.as_str(), config.port as u16)),
        )
        .await
        .map_err(|_| anyhow::anyhow!("timed out connecting to postgres"))??;
        tcp.set_nodelay(true)?;
        Ok((connect_verified(config, tcp).await?, None))
    } else {
        let connection_string = get_pg_connection_string(config);

        let mut tls_config = ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::aws_lc_rs::default_provider(),
        ))
        .with_protocol_versions(&[&rustls::version::TLS13])?
        .with_root_certificates(RootCertStore::empty())
        .with_no_client_auth();
        tls_config
            .dangerous()
            .set_certificate_verifier(Arc::new(NoCertificateVerification));
        let tls_connector = MakeRustlsConnect::new(tls_config);
        let (client, connection) = tokio_postgres::connect(&connection_string, tls_connector)
            .await
            .map_err(|e| {
                anyhow::anyhow!("error encountered while connecting to postgres {:?}", e)
            })?;
        tokio::task::spawn(async move {
            if let Err(e) = connection.await {
                tracing::info!("connection error: {}", e)
            }
        });
        Ok((client, None))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TEST_CA: &str = include_str!("../testdata/rds-ca-rsa2048-g1.pem");

    fn pg(host: &str) -> PostgresConfig {
        PostgresConfig {
            host: host.to_string(),
            ..Default::default()
        }
    }

    #[test]
    fn verifies_tls_like_flow() {
        assert!(!verifies_tls(&pg("db.example.com")));
        assert!(verifies_tls(&PostgresConfig {
            require_tls: true,
            ..pg("db.example.com")
        }));
        assert!(verifies_tls(&PostgresConfig {
            root_ca: Some(TEST_CA.to_string()),
            ..pg("db.example.com")
        }));
        assert!(!verifies_tls(&PostgresConfig {
            require_tls: true,
            disable_tls: Some(true),
            ..pg("db.example.com")
        }));
    }

    #[test]
    fn server_name_prefers_tls_host() {
        assert_eq!(tls_server_name(&pg("db.example.com")), "db.example.com");
        assert_eq!(
            tls_server_name(&PostgresConfig {
                tls_host: " cert.example.com ".to_string(),
                ..pg("10.0.0.1")
            }),
            "cert.example.com"
        );
    }

    #[test]
    fn root_ca_must_parse() {
        let config = |root_ca: &str| PostgresConfig {
            require_tls: true,
            root_ca: Some(root_ca.to_string()),
            ..pg("db.example.com")
        };
        assert!(verified_tls_config(&config(TEST_CA)).is_ok());
        assert!(verified_tls_config(&config("not a certificate")).is_err());
        assert!(
            verified_tls_config(&PostgresConfig {
                require_tls: true,
                ..pg("db.example.com")
            })
            .is_ok()
        );
    }
}
