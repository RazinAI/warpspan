import { Provider } from '@/components/provider';
import type { Metadata, Viewport } from 'next';
import { IBM_Plex_Mono, Inter } from 'next/font/google';
import { siteUrl } from '@/lib/shared';
import './global.css';

// The Razin site's faces: Inter for text, IBM Plex Mono for code and labels.
const inter = Inter({
  subsets: ['latin'],
  variable: '--font-inter',
});
const plexMono = IBM_Plex_Mono({
  subsets: ['latin'],
  weight: ['400', '500', '600'],
  variable: '--font-plex-mono',
});

export const metadata: Metadata = {
  metadataBase: new URL(process.env.NEXT_PUBLIC_SITE_URL ?? siteUrl),
  title: {
    default: 'Warpspan docs',
    template: '%s · Warpspan docs',
  },
  description:
    'Warpspan records the CUDA kernels a service runs, tags each one with the OpenTelemetry span that launched it, and exports them as OTLP profiles for Pyroscope and Grafana.',
};

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: dark)', color: '#16130f' },
    { media: '(prefers-color-scheme: light)', color: '#f6f2e8' },
  ],
};

export default function Layout({ children }: LayoutProps<'/'>) {
  return (
    <html
      lang="en"
      className={`${inter.variable} ${plexMono.variable}`}
      suppressHydrationWarning
    >
      <body className="flex min-h-screen flex-col">
        <Provider>{children}</Provider>
      </body>
    </html>
  );
}
