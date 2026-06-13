// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  docsSidebar: [
    {
      type: 'doc',
      id: 'index',
      label: 'Overview',
    },
    {
      type: 'doc',
      id: 'cluster-lifecycle',
      label: 'Cluster Lifecycle',
    },
    {
      type: 'doc',
      id: 'replication-failover',
      label: 'Replication and Failover',
    },
    {
      type: 'doc',
      id: 'backup-recovery',
      label: 'Physical Backup and Recovery',
    },
    {
      type: 'doc',
      id: 'scheduled-backups',
      label: 'Scheduled Backups',
    },
    {
      type: 'doc',
      id: 'pitr',
      label: 'Point-In-Time Recovery',
    },
  ],
};

module.exports = sidebars;
