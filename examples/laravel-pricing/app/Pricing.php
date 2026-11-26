<?php

declare(strict_types=1);

namespace PricingLab;

use Illuminate\Support\Collection;

final class Pricing
{
    public function quote(int $units): int
    {
        return LegacyAmount::normalize($units);
    }

    public function credit(int $units): int
    {
        return LegacyAmount::normalize($units);
    }

    public function balance(int $order, int $credit): int
    {
        return (new Collection([$this->quote($order), $this->credit($credit)]))->sum();
    }
}
