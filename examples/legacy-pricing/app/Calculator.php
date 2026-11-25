<?php

declare(strict_types=1);

namespace UpgradeLab\Pricing;

use UpgradeLab\Pricing\LegacyPrice as Legacy;

final class LegacyPrice
{
    public static function estimate(int $subtotal, int $adjustment): int
    {
        return $subtotal + $adjustment;
    }
}

final class Quote
{
    public static function estimate(int $subtotal, int $adjustment): int
    {
        return max(0, $subtotal + $adjustment);
    }
}

final class Alternative
{
    public static function estimate(int $value): int
    {
        return $value;
    }
}

function estimateSale(int $subtotal, int $discount): int
{
    return Legacy::estimate($subtotal, -$discount);
}

function estimateCredit(int $subtotal, int $credit): int
{
    // Credits may cross zero until credit notes are migrated in a separate batch.
    return \UpgradeLab\Pricing\LegacyPrice::estimate($subtotal, -$credit);
}

function diagnostic(): int
{
    return Alternative::estimate(7);
}
