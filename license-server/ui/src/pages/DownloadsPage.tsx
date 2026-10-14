// /downloads (signed in): links to public release assets and GHCR packages.
// GitHub's latest release URL and unversioned Helm pulls follow stable
// releases without fetching metadata or routing downloads through the portal.
// Versioned archives are selected on the release page.
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';

const OWNER = 'robertherbaugh';
const REGISTRY = `ghcr.io/${OWNER}`;
const REPOSITORY = `https://github.com/${OWNER}/kudurru`;
const LATEST_RELEASE = `${REPOSITORY}/releases/latest`;
const PACKAGES = `https://github.com/users/${OWNER}/packages/container/package`;
const DEPLOY_GUIDES = 'https://docs.kudurru.io/deploy';

interface DownloadLink {
  label: string;
  href: string;
}

interface PackageRow extends Record<string, unknown> {
  id: string;
  name: string;
  description: string;
  downloads: DownloadLink[];
  guides: DownloadLink[];
}

const packages: PackageRow[] = [
  {
    id: 'helm',
    name: 'Helm charts',
    description: 'The kudurru server and its separate CRD chart, published to GHCR.',
    downloads: [
      { label: 'Server chart', href: `${PACKAGES}/charts%2Fkudurru` },
      { label: 'CRD chart', href: `${PACKAGES}/charts%2Fkudurru-crds` },
    ],
    guides: [{ label: 'Helm guide', href: `${DEPLOY_GUIDES}/helm/` }],
  },
  {
    id: 'containers',
    name: 'Container images',
    description: 'Linux amd64 and arm64 images, with debug, host-network and edge variants.',
    downloads: [
      { label: 'Server images', href: `${PACKAGES}/kudurru` },
      { label: 'dnsdist images', href: `${PACKAGES}/kudurru-dnsdist` },
      { label: 'keepalived images', href: `${PACKAGES}/kudurru-keepalived` },
    ],
    guides: [{ label: 'Docker guide', href: `${DEPLOY_GUIDES}/docker/` }],
  },
  {
    id: 'compose',
    name: 'Docker Compose',
    description: 'Single-node, single-node with edge, and three-host HA configurations. Choose the kudurru_<version>_compose.tar.gz release asset.',
    downloads: [{ label: 'Compose release bundle', href: LATEST_RELEASE }],
    guides: [
      { label: 'Single-node guide', href: `${DEPLOY_GUIDES}/docker/` },
      { label: 'HA guide', href: `${DEPLOY_GUIDES}/compose-ha/` },
    ],
  },
  {
    id: 'binaries',
    name: 'Binaries and Linux packages',
    description: 'Linux and macOS archives for amd64 and arm64, plus DEB, RPM and APK packages. Choose your OS and architecture from the release assets.',
    downloads: [{ label: 'Binaries and packages', href: LATEST_RELEASE }],
    guides: [{ label: 'Linux package guide', href: `${DEPLOY_GUIDES}/docker/#linux-packages-and-systemd` }],
  },
  {
    id: 'manifests',
    name: 'Kubernetes manifests',
    description: 'CRD definitions and the Kubernetes installation manifest from the latest stable release.',
    downloads: [
      { label: 'crds.yaml', href: `${LATEST_RELEASE}/download/crds.yaml` },
      { label: 'install.yaml', href: `${LATEST_RELEASE}/download/install.yaml` },
    ],
    guides: [{ label: 'Kustomize guide', href: `${DEPLOY_GUIDES}/kustomize/` }],
  },
];

function ExternalLinks({ links }: { links: DownloadLink[] }) {
  return (
    <VStack gap={1}>
      {links.map((link) => <Link key={link.href} href={link.href} isExternalLink isStandalone>{link.label}</Link>)}
    </VStack>
  );
}

const columns: TableColumn<PackageRow>[] = [
  {
    key: 'name',
    header: 'Package',
    width: proportional(3, { minWidth: 240 }),
    renderCell: (item) => (
      <VStack gap={1}>
        <Text weight="semibold">{item.name}</Text>
        <Text type="supporting">{item.description}</Text>
      </VStack>
    ),
  },
  { key: 'downloads', header: 'Downloads and versions', width: proportional(2, { minWidth: 180 }), renderCell: (item) => <ExternalLinks links={item.downloads} /> },
  { key: 'guides', header: 'Installation', width: proportional(1, { minWidth: 160 }), renderCell: (item) => <ExternalLinks links={item.guides} /> },
];

export default function DownloadsPage() {
  return (
    <Page>
      <PageHeader
        title="Downloads"
        description="Get kudurru packages from GitHub Releases and the GitHub Container Registry."
        actions={
          <>
            <Link href={LATEST_RELEASE} isExternalLink isStandalone>Latest stable release</Link>
            <Link href={`${REPOSITORY}/releases`} isExternalLink isStandalone>All releases</Link>
          </>
        }
      />
      <Panel title="Packages">
        <Table<PackageRow> data={packages} columns={columns} idKey="id" verticalAlign="top" />
      </Panel>
      <Panel title="Pull a container image">
        <CodeBlock code={`docker pull ${REGISTRY}/kudurru:latest`} language="bash" isWrapped width="100%" size="sm" />
        <Text type="supporting" as="p">For a specific release, replace <Text type="code">latest</Text> with its version, without the leading <Text type="code">v</Text>.</Text>
      </Panel>
      <Panel title="Download Helm charts">
        <CodeBlock
          code={`helm pull oci://${REGISTRY}/charts/kudurru-crds\nhelm pull oci://${REGISTRY}/charts/kudurru`}
          language="bash"
          isWrapped
          width="100%"
          size="sm"
        />
        <Text type="supporting" as="p">These commands download the latest stable chart archives. To choose a release, add <Text type="code">--version &lt;version&gt;</Text> to both commands. Use matching chart and container versions.</Text>
      </Panel>
      <Panel title="Verify your download">
        <Text as="p">Use the checksums and signatures from the same release as your download. The deployment guide explains how to verify packages and container images.</Text>
        <HStack gap={3} wrap="wrap">
          <Link href={`${LATEST_RELEASE}/download/checksums.txt`} isExternalLink isStandalone>Latest release checksums</Link>
          <Link href={`${DEPLOY_GUIDES}/#release-artifacts`} isExternalLink isStandalone>Verification guide</Link>
        </HStack>
      </Panel>
      <Text type="supporting" as="p">Find your license key under <Link href="/my/licenses">My licenses</Link>. The installation guides explain how to add it to your deployment.</Text>
    </Page>
  );
}
