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

/** `console.tab` extensions with this context ID become tabs on the Nodes list page. */
export const NODES_LIST_TAB_CONTEXT_ID = 'nodes-list';

/**
 * Query parameter that selects the tab. Tabs cannot be path segments, because
 * `/k8s/cluster/nodes/<name>` is the Node details route.
 */
const TAB_PARAM = 'tab';

/**
 * Renders the Nodes list page with a tab bar for plugin tabs. Without plugin tabs it
 * renders `renderNodesTab()` alone, so the page is unchanged. A plugin tab replaces
 * the built-in list entirely, so the list's watches and metric polling stay unmounted.
 */
export const NodesListTabs: FC<NodesListTabsProps> = ({ renderNodesTab }) => {
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

  if (tabs.length === 0) {
    return <>{renderNodesTab()}</>;
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

  // the div wrapper prevents the tabs from collapsing in a flexbox
  const tabBar = (
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
  );

  if (!activeTab) {
    return <>{renderNodesTab(tabBar)}</>;
  }

  return (
    <>
      <DocumentTitle>{activeTab.properties.name}</DocumentTitle>
      <ListPageHeader title={t('Nodes')} />
      {tabBar}
      <ErrorBoundaryPage key={activeTab.uid}>
        <AsyncComponent loader={activeTab.properties.component} />
      </ErrorBoundaryPage>
    </>
  );
};

type NodesListTabsProps = {
  /** Renders the built-in Nodes tab, with the tab bar to place under its header. */
  renderNodesTab: (tabBar?: ReactNode) => ReactNode;
};
