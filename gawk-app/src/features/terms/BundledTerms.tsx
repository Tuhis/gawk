import { getOperatorContact, getOperatorName, getTermsVersion } from '../../config';

// R23 (docs/29 §7). The shipped default Terms of Use. This component is always
// in the bundle, so a dev build, an un-configured install, and every
// override-fetch failure render real, styled content with zero network
// dependency. Operator name/contact/version are substituted from runtime
// config; an operator who wants different prose overrides the whole body via
// config.termsUrl (see TermsPage).
//
// This text is a protective template written to the operator's stated
// priorities — it is NOT legal advice. For a deployment exposed beyond a
// closed circle of known people, have a Finnish lawyer review it (see the
// gawk-app chart README and docs/29 §0).
export function BundledTerms() {
  const operator = getOperatorName();
  const contact = getOperatorContact();
  const version = getTermsVersion();

  return (
    <>
      <h1>Terms of Use</h1>
      <p className="effective">Effective version: {version}</p>

      <p>
        These Terms of Use (&ldquo;<strong>Terms</strong>&rdquo;) govern your access to and use of
        this self-hosted streaming service (the &ldquo;<strong>Service</strong>&rdquo;), which is
        operated by <strong>{operator}</strong> (the &ldquo;<strong>Operator</strong>&rdquo;). The
        Service lets a broadcaster share their screen and lets viewers watch it in a web browser by
        entering a join code. The Service uses no accounts and no login.{' '}
        <strong>
          By using the Service — as a broadcaster or as a viewer — you agree to these Terms. If you
          do not agree, do not use the Service.
        </strong>
      </p>

      <h2>1. The Service, provided &ldquo;as is.&rdquo;</h2>
      <p>
        The Service is a private, self-hosted deployment made available at the Operator&rsquo;s
        discretion, primarily for use among people known to the Operator. It is provided{' '}
        <strong>&ldquo;as is&rdquo; and &ldquo;as available,&rdquo; with no warranties of any kind</strong>,
        whether express, implied, or statutory, including without limitation any implied warranties
        of merchantability, fitness for a particular purpose, non-infringement, availability,
        uptime, quality, or reliability. The Operator does not warrant that the Service will be
        uninterrupted, timely, secure, error-free, or that any content will be transmitted without
        delay, loss, or degradation.
      </p>

      <h2>2. Eligibility.</h2>
      <p>
        You must be at least <strong>18 years of age</strong> to use the Service. If you are under
        18, you may use the Service{' '}
        <strong>only with the consent and under the supervision of a parent or legal guardian</strong>,
        who accepts these Terms on your behalf and is responsible for your use. By using the Service
        you represent that you meet these requirements.
      </p>

      <h2>3. How the Service works; the join code.</h2>
      <p>
        Starting a broadcast produces a short join code.{' '}
        <strong>Anyone who holds the join code can view the broadcast.</strong> The code is the only
        access control for viewing; the Operator does not verify the identity of viewers. You are
        responsible for deciding with whom you share a join code and for the consequences of sharing
        it.
      </p>

      <h2>4. Your content and your responsibility for it.</h2>
      <p>
        As a broadcaster, you are{' '}
        <strong>solely and fully responsible for everything you transmit through the Service</strong>{' '}
        — your screen, applications, audio, and anything visible or audible within them (your
        &ldquo;<strong>Content</strong>&rdquo;). You represent and warrant that you have all rights,
        licenses, and permissions necessary to transmit your Content and to allow it to be viewed by
        those who hold the join code, and that your Content does not infringe or violate the rights
        of any third party.
      </p>

      <h2>5. Lawful use.</h2>
      <p>
        You may transmit only Content that is{' '}
        <strong>lawful both in Finland and in the country or jurisdiction from which you use the Service.</strong>{' '}
        You are responsible for knowing and complying with the laws that apply to you. Without
        limiting the foregoing, you must not use the Service to transmit, request, or facilitate any
        Content or conduct that:
      </p>
      <ul>
        <li>is illegal, or promotes, facilitates, or depicts illegal acts;</li>
        <li>
          infringes any copyright, trademark, trade secret, privacy, publicity, or other right of
          any person;
        </li>
        <li>discloses another person&rsquo;s private or personal information without their consent;</li>
        <li>is defamatory, harassing, threatening, hateful, or intended to intimidate;</li>
        <li>
          is sexually explicit involving minors, or exploits or endangers minors in any way;
        </li>
        <li>
          contains malware, or is intended to disrupt, damage, or gain unauthorized access to any
          system, network, or data;
        </li>
        <li>
          attempts to circumvent, overload, or interfere with the Service, its limits, or its
          security; or
        </li>
        <li>violates any applicable law or regulation.</li>
      </ul>

      <h2>6. Monitoring, moderation, and content management.</h2>
      <p>
        The Operator reserves the right,{' '}
        <strong>
          at its sole discretion and without notice or liability, to monitor, inspect, analyze,
          record, retain, block, interrupt, throttle, suspend, or remove any Content or broadcast,
          and to restrict or terminate any person&rsquo;s access to the Service
        </strong>
        , for any reason — including to maintain service quality, to enforce these Terms, to comply
        with law, and to exercise the Operator&rsquo;s own judgement as to what is unfit for the
        Service. The Operator is under <strong>no obligation</strong> to monitor Content, and any
        decision to do so, or not to, does not create any duty or liability.
      </p>

      <h2>7. Recording and data.</h2>
      <p>
        <em>As the Service is currently operated,</em> it relays broadcast media in real time and{' '}
        <strong>does not persistently record or store broadcast media</strong>; any technical caches
        used to relay a stream are short-lived, held only in memory, and not retained.{' '}
        <strong>
          This paragraph describes present operation only. It is not a warranty and not a commitment
          against change
        </strong>
        , and it does not limit the rights the Operator reserves in Section 6. The Service may
        process limited technical data necessary to operate it (for example, join codes and network
        connection information such as IP addresses, which may appear transiently in operational
        logs). To the extent any such data is personal data, it is processed only to provide and
        protect the Service; questions about personal data may be directed to the contact in Section
        13.
      </p>
      <p>
        The Service also collects <strong>technical performance measurements</strong> from
        broadcasters and viewers, automatically and for every session, in order to diagnose
        streaming problems. What is collected is limited to playback and network statistics — for
        example frame rates, latency, buffering, packet loss, reconnects, and error codes — together
        with a coarse browser and operating-system category (such as &ldquo;Chrome 152&rdquo; and
        &ldquo;Windows&rdquo;), a per-session identifier that exists only for the duration of that
        session, and an obscured form of the broadcast code. It{' '}
        <strong>
          does not include any broadcast audio or video, any screen contents, your IP address, your
          full browser user-agent string, any account or contact details, or any identifier that
          follows you between sessions or between broadcasts
        </strong>
        . These measurements are retained in full for a short period (currently 14 days) and
        thereafter only as aggregate per-session summaries. Like Section 6, this paragraph describes
        present operation rather than a warranty.
      </p>

      <h2>8. Limitation of liability.</h2>
      <p>
        To the maximum extent permitted by applicable law, the Operator (and anyone involved in
        providing the Service) shall <strong>not be liable</strong> for any indirect, incidental,
        special, consequential, exemplary, or punitive damages, or for any loss of profits, data,
        goodwill, or other intangible losses, arising out of or relating to your use of or inability
        to use the Service, whatever the cause and under any theory of liability. To the maximum
        extent permitted by applicable law, the Operator&rsquo;s total aggregate liability arising
        out of or relating to the Service is limited to <strong>zero euros</strong>. Nothing in
        these Terms excludes or limits liability that cannot be excluded or limited under mandatory
        applicable law.
      </p>

      <h2>9. Your indemnity.</h2>
      <p>
        To the maximum extent permitted by applicable law, you agree to{' '}
        <strong>indemnify, defend, and hold harmless the Operator</strong> from and against any
        claims, demands, damages, losses, liabilities, costs, and expenses (including reasonable
        legal fees) arising out of or relating to your Content, your use of the Service, or your
        breach of these Terms or of any law or third-party right.
      </p>

      <h2>10. Availability, suspension, and termination.</h2>
      <p>
        The Service may be changed, limited, suspended, or discontinued at any time, in whole or in
        part, with or without notice. The Operator may{' '}
        <strong>deny, suspend, or terminate access for anyone at any time and for any reason</strong>,
        including suspected violation of these Terms. There is no entitlement to the Service and no
        guarantee of continued availability.
      </p>

      <h2>11. Changes to these Terms.</h2>
      <p>
        The Operator may update these Terms from time to time. Material changes are indicated by a
        change to the effective version shown above, and continued use of the Service after such a
        change constitutes acceptance of the updated Terms.
      </p>

      <h2>12. Governing law.</h2>
      <p>These Terms are governed by the laws of Finland.</p>

      <h2>13. Contact.</h2>
      <p>
        Questions about these Terms or the Service may be directed to{' '}
        {contact ? <strong>{contact}</strong> : <span>the Operator of this deployment</span>}.
      </p>

      <h2>14. Miscellaneous.</h2>
      <p>
        If any provision of these Terms is held unenforceable, the remaining provisions remain in
        full force. The Operator&rsquo;s failure to enforce any provision is not a waiver of it.
        These Terms are the entire agreement between you and the Operator regarding the Service and
        supersede any prior understanding on that subject. Section headings are for convenience
        only.
      </p>
    </>
  );
}
