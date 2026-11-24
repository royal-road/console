import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { useExtensions } from '@console/plugin-sdk/src/api/useExtensions';
import { NODES_LIST_TAB_CONTEXT_ID, NodesListTabs } from '../NodesListTabs';

jest.mock('@console/plugin-sdk/src/api/useExtensions', () => ({
  useExtensions: jest.fn(),
}));

jest.mock('@console/internal/components/factory/ListPage/ListPageHeader', () => ({
  __esModule: true,
  default: ({ title, children }) => (
    <>
      <h1>{title}</h1>
      {children}
    </>
  ),
}));

jest.mock('../NodeGroupEditButton', () => ({
  __esModule: true,
  default: () => <button type="button">Edit node groups</button>,
}));

const useExtensionsMock = useExtensions as jest.Mock;

const navTab = (contextId: string, name: string, href: string) => ({
  type: 'console.tab',
  uid: `${contextId}-${href}`,
  properties: {
    contextId,
    name,
    href,
    component: () => Promise.resolve(() => <div>{`${name} content`}</div>),
  },
});

const renderAt = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <NodesListTabs>
        <div>built-in node list</div>
      </NodesListTabs>
    </MemoryRouter>,
  );

describe('NodesListTabs', () => {
  it('renders the node list without a tab bar when no nodes-list tabs exist', () => {
    useExtensionsMock.mockReturnValue([navTab('dev-console-observe', 'Other', 'other')]);
    renderAt('/k8s/cluster/nodes?tab=other');

    expect(screen.getByText('built-in node list')).toBeVisible();
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Nodes' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Edit node groups' })).toBeVisible();
  });

  it('renders the plugin tab named by the tab query parameter instead of the node list', async () => {
    useExtensionsMock.mockReturnValue([
      navTab(NODES_LIST_TAB_CONTEXT_ID, 'External nodes', 'external'),
    ]);
    renderAt('/k8s/cluster/nodes?tab=external&host=db1');

    expect(await screen.findByText('External nodes content')).toBeVisible();
    expect(screen.queryByText('built-in node list')).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Nodes' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Edit node groups' })).toBeVisible();
    expect(screen.getByRole('tab', { name: 'External nodes' })).toHaveAttribute(
      'href',
      '/k8s/cluster/nodes?tab=external',
    );
  });

  it('renders the node list with the tab bar when the tab parameter is unknown', () => {
    useExtensionsMock.mockReturnValue([
      navTab(NODES_LIST_TAB_CONTEXT_ID, 'Zeta', 'zeta'),
      navTab(NODES_LIST_TAB_CONTEXT_ID, 'External nodes', 'external'),
    ]);
    renderAt('/k8s/cluster/nodes?tab=missing');

    expect(screen.getByText('built-in node list')).toBeVisible();
    expect(screen.getAllByRole('tab').map((link) => link.textContent)).toEqual([
      'Nodes',
      'External nodes',
      'Zeta',
    ]);
    expect(screen.getByRole('tab', { name: 'Nodes' })).toHaveAttribute(
      'href',
      '/k8s/cluster/nodes',
    );
  });
});
