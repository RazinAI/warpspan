import type { BaseLayoutProps } from 'fumadocs-ui/layouts/shared';
import { WarpspanMark } from '@/components/mark';
import { repoUrl } from './shared';

export function baseOptions(): BaseLayoutProps {
  return {
    nav: {
      title: (
        <span className="font-ui inline-flex items-center gap-2.5 text-[15px] font-semibold">
          <WarpspanMark className="size-5" />
          <span>Warpspan</span>
          <span className="font-normal text-fd-muted-foreground">docs</span>
        </span>
      ),
    },
    links: [
      { text: 'Docs', url: '/docs', active: 'nested-url' },
      { text: 'Configuration', url: '/docs/reference/configuration' },
      { text: 'GitHub', url: repoUrl, external: true },
    ],
  };
}
