import type { FC, ReactNode } from 'react';
import { useMemo } from 'react';
import { Tab, Tabs, TabTitleText } from '@patternfly/react-core';
import { useTranslation } from 'react-i18next';
import { useLocation, useNavigate, useSearchParams } from 'react-router';
import type { NavTab } from '@console/dynamic-plugin-sdk/src/extensions/horizontal-nav-tabs';
import { isTab } from '@console/dynamic-plugin-sdk/src/extensions/horizontal-nav-tabs';
import ListPageHeader from '@console/internal/components/factory/ListPage/ListPageHeader';
import { AsyncComponent } from '@console/internal/components/utils/async';
import { useExtensions } from '@console/plugin-sdk/src/api/useExtensions';
import { DocumentTitle } from '@console/shared/src/components/document-title/DocumentTitle';
import { ErrorBoundaryPage } from '@console/shared/src/components/error/fallbacks/ErrorBoundaryPage';
import NodeGroupEditButton from './NodeGroupEditButton';

/** `console.tab` extensions with this context ID become tabs on the Nodes list page. */
export const NODES_LIST_TAB_CONTEXT_ID = 'nodes-list';

/**
 * Query parameter that selects the tab. Tabs cannot be path segments, because
 * `/k8s/cluster/nodes/<name>` is the Node details route.
 */
const TAB_PARAM = 'tab';

/**
 * Renders the Nodes list page header, a tab bar for plugin tabs, and the active tab.
 * Without plugin tabs it renders the header and `children` (the built-in list), so the
 * page is unchanged. A plugin tab replaces `children` entirely, so the list's watches
 * and metric polling stay unmounted.
 */
export const NodesListTabs: FC<NodesListTabsProps> = ({ children }) => {
  const { t } = useTranslation('console-app');
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const [searchParams] = useSearchParams();
  const tabExtensions = useExtensions<NavTab>(isTab);
  const tabs = useMemo(
    () =>
      tabExtensions
        .filter((tab) => tab.properties.contextId === NODES_LIST_TAB_CONTEXT_ID)
        .sort((a, b) => a.properties.name.localeCompare(b.properties.name)),
    [tabExtensions],
  );

  const header = (
    <ListPageHeader title={t('Nodes')}>
      <NodeGroupEditButton />
    </ListPageHeader>
  );

  if (tabs.length === 0) {
    return (
      <>
        {header}
        {children}
      </>
    );
  }

  const activeTab = tabs.find((tab) => tab.properties.href === searchParams.get(TAB_PARAM));
  const links = [
    { key: '', name: t('Nodes'), to: pathname },
    ...tabs.map(({ properties: { name, href } }) => ({
      key: href,
      name,
      to: `${pathname}?${new URLSearchParams({ [TAB_PARAM]: href }).toString()}`,
    })),
  ];

  return (
    <>
      {header}
      {/* Mirrors the tab bar in `NavBar` (public/components/utils/horizontal-nav.tsx), which
          only supports path-based tabs; keep the two in sync.
          The div wrapper prevents the tabs from collapsing in a flexbox. */}
      <div>
        <Tabs activeKey={activeTab?.properties.href ?? ''} component="nav" usePageInsets>
          {links.map(({ key, name, to }) => (
            <Tab
              key={key}
              eventKey={key}
              href={to}
              onClick={(e) => {
                e.preventDefault();
                navigate(to);
              }}
              title={<TabTitleText>{name}</TabTitleText>}
              data-test={`nodes-list-tab-${key || 'nodes'}`}
              aria-controls={undefined} // there is no tab panel; the content follows the nav
            />
          ))}
        </Tabs>
      </div>
      {activeTab ? (
        <>
          <DocumentTitle>{activeTab.properties.name}</DocumentTitle>
          <ErrorBoundaryPage key={activeTab.uid}>
            <AsyncComponent loader={activeTab.properties.component} />
          </ErrorBoundaryPage>
        </>
      ) : (
        children
      )}
    </>
  );
};

type NodesListTabsProps = {
  /** The built-in Nodes tab content, rendered under the header and tab bar. */
  children: ReactNode;
};
